package memory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

type segMode uint8

const (
	segEntries segMode = iota
	segNative
	segKnowledge
)

type corpusFile struct {
	Bank, Path string
	Mode       segMode
}

// corpusOf lists bank, archive, native and knowledge files in that order without reading file contents.
func corpusOf() []corpusFile {
	var out []corpusFile
	for _, name := range bankFiles {
		out = append(out, corpusFile{"home", Path(name), segEntries})
	}
	for _, path := range ownedFiles("archive") {
		out = append(out, corpusFile{"archive", path, segEntries})
	}
	for _, path := range topicFiles() {
		out = append(out, corpusFile{"native", path, segNative})
	}
	for _, path := range searchTopicFiles(SubDir(techdebtDir)) {
		out = append(out, corpusFile{techdebtDir, path, segNative})
	}
	for _, path := range knowledgeNotes() {
		out = append(out, corpusFile{"knowledge", path, segKnowledge})
	}
	return out
}

// ownedFiles lists the *.md in the store folder sub; sub "" is the top-level files.
func ownedFiles(sub string) []string { return transcript.GlobIn(SubDir(sub), "*.md") }

// topicFiles are the native topics: top-level non-typed notes plus the feedback/ and notes/ folders.
func topicFiles() []string {
	files := searchTopicFiles(StoreDir())
	for _, d := range noteDirs {
		if d == techdebtDir {
			continue
		}
		files = append(files, searchTopicFiles(SubDir(d))...)
	}
	return files
}

func searchTopicFiles(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var files []string
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if reservedNames[name] {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files
}

func parseBlocks(mode segMode, base string, data []byte) []string {
	s := strings.ReplaceAll(string(data), "\r\n", "\n") // the frontmatter parsers anchor on a bare newline
	switch mode {
	case segEntries:
		_, blocks := entryBlocks(logicalName(base), s)
		return blocks
	case segNative:
		if frontmatterValue(s, "harness_kind") == "bank" {
			return nil
		}
		if name, desc, body, ok := topicFrontmatter(s); ok {
			return []string{name + " — " + desc + "\n" + body}
		}
	case segKnowledge:
		return []string{strings.TrimSpace(stripFrontmatter(s))}
	}
	return nil
}

// segView stores little-endian tables followed by a shared term/text blob.
type segView []byte

// A document row is dl, text offset, text length, then the block's blockSum and ref, both fixed at encode time.
const (
	segHeader = 12
	refWidth  = 9 // an 8-hex id, or "z" and 8 hex; the 8-hex form is NUL-padded
	docStride = 20 + refWidth
)

type segDoc struct {
	text string
	dl   uint32
}

type segPosting struct {
	doc, tf uint32
}

// indexBlocks builds each indexable block's document and the per-term postings; blocks with no terms are dropped.
func indexBlocks(blocks []string) (docs []segDoc, postings map[string][]segPosting, sumdl uint64, nposts int) {
	postings = map[string][]segPosting{}
	for _, block := range blocks {
		tf := map[string]uint32{}
		var dl uint32
		first, rest, _ := strings.Cut(block, "\n")
		for _, t := range contentTerms(first) {
			tf[t] += searchTitleWeight
			dl += searchTitleWeight
		}
		for _, t := range contentTerms(rest) {
			tf[t]++
			dl++
		}
		if dl == 0 {
			continue
		}
		for term, count := range tf {
			postings[term] = append(postings[term], segPosting{uint32(len(docs)), count})
			nposts++
		}
		docs = append(docs, segDoc{block, dl})
		sumdl += uint64(dl)
	}
	return docs, postings, sumdl, nposts
}

func encodeSegment(blocks []string) segView {
	docs, postings, sumdl, nposts := indexBlocks(blocks)
	terms := make([]string, 0, len(postings))
	for term := range postings {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	termStart := segHeader + len(docs)*docStride
	postStart := termStart + 4 + len(terms)*16
	blobStart := postStart + 4 + nposts*8
	s := make(segView, blobStart)
	binary.LittleEndian.PutUint32(s, uint32(len(docs)))
	binary.LittleEndian.PutUint64(s[4:], sumdl)
	binary.LittleEndian.PutUint32(s[termStart:], uint32(len(terms)))
	binary.LittleEndian.PutUint32(s[postStart:], uint32(nposts))
	postOff := 0
	for i, term := range terms {
		off := termStart + 4 + i*16
		binary.LittleEndian.PutUint32(s[off:], uint32(len(s)-blobStart))
		binary.LittleEndian.PutUint32(s[off+4:], uint32(len(term)))
		binary.LittleEndian.PutUint32(s[off+8:], uint32(postOff))
		binary.LittleEndian.PutUint32(s[off+12:], uint32(len(postings[term])))
		s = append(s, term...)
		for _, p := range postings[term] {
			binary.LittleEndian.PutUint32(s[postStart+4+postOff:], p.doc)
			binary.LittleEndian.PutUint32(s[postStart+8+postOff:], p.tf)
			postOff += 8
		}
	}
	for i, d := range docs {
		off := segHeader + i*docStride
		binary.LittleEndian.PutUint32(s[off:], d.dl)
		binary.LittleEndian.PutUint32(s[off+4:], uint32(len(s)-blobStart))
		binary.LittleEndian.PutUint32(s[off+8:], uint32(len(d.text)))
		binary.LittleEndian.PutUint64(s[off+12:], blockSum(d.text))
		copy(s[off+20:off+20+refWidth], blockRef(d.text, ""))
		s = append(s, d.text...)
	}
	return s
}

func (s segView) Docs() int {
	return int(binary.LittleEndian.Uint32(s))
}

func (s segView) sumDL() uint64 {
	return binary.LittleEndian.Uint64(s[4:])
}

func (s segView) Doc(i int) (text string, dl uint32) {
	termStart := segHeader + s.Docs()*docStride
	postStart := termStart + 4 + int(binary.LittleEndian.Uint32(s[termStart:]))*16
	blobStart := postStart + 4 + int(binary.LittleEndian.Uint32(s[postStart:]))*8
	off := segHeader + i*docStride
	start := blobStart + int(binary.LittleEndian.Uint32(s[off+4:]))
	end := start + int(binary.LittleEndian.Uint32(s[off+8:]))
	return string(s[start:end]), binary.LittleEndian.Uint32(s[off:])
}

// DocRef is document i's blockSum and blockRef(text, ""), read from the row instead of rehashing the text.
func (s segView) DocRef(i int) (sum uint64, ref string) {
	off := segHeader + i*docStride
	r := s[off+20 : off+20+refWidth]
	if r[refWidth-1] == 0 {
		r = r[:refWidth-1]
	}
	return binary.LittleEndian.Uint64(s[off+12:]), string(r)
}

// Posting offsets are byte offsets relative to the posting table; text and term offsets are blob-relative.
func (s segView) lookup(term string) (posts []byte, n int) {
	termStart := segHeader + s.Docs()*docStride
	nterms := int(binary.LittleEndian.Uint32(s[termStart:]))
	postStart := termStart + 4 + nterms*16
	blobStart := postStart + 4 + int(binary.LittleEndian.Uint32(s[postStart:]))*8
	i := sort.Search(nterms, func(i int) bool {
		off := termStart + 4 + i*16
		start := blobStart + int(binary.LittleEndian.Uint32(s[off:]))
		end := start + int(binary.LittleEndian.Uint32(s[off+4:]))
		return string(s[start:end]) >= term
	})
	if i == nterms {
		return nil, 0
	}
	off := termStart + 4 + i*16
	termOff := blobStart + int(binary.LittleEndian.Uint32(s[off:]))
	termEnd := termOff + int(binary.LittleEndian.Uint32(s[off+4:]))
	if string(s[termOff:termEnd]) != term {
		return nil, 0
	}
	start := postStart + 4 + int(binary.LittleEndian.Uint32(s[off+8:]))
	n = int(binary.LittleEndian.Uint32(s[off+12:]))
	return s[start : start+n*8], n
}

func (s segView) Valid() bool {
	l, ok := s.layout()
	return ok && s.docsValid(l) && s.termsValid(l) && s.postsValid(l)
}

// segLayout is a segment's table sizes and offsets as read from its header.
type segLayout struct {
	ndocs, nterms, nposts           uint64
	termStart, postStart, blobStart uint64
	blobLen                         uint64
}

func (s segView) u32(off uint64) uint64 { return uint64(binary.LittleEndian.Uint32(s[off:])) }

// layout reads the header, reporting false when a table would run past the end of the segment.
func (s segView) layout() (l segLayout, ok bool) {
	size := uint64(len(s))
	if size < 12 {
		return l, false
	}
	l.ndocs = s.u32(0)
	l.termStart = segHeader + l.ndocs*docStride
	if l.termStart+4 > size {
		return l, false
	}
	l.nterms = s.u32(l.termStart)
	l.postStart = l.termStart + 4 + l.nterms*16
	if l.postStart+4 > size {
		return l, false
	}
	l.nposts = s.u32(l.postStart)
	l.blobStart = l.postStart + 4 + l.nposts*8
	if l.blobStart > size {
		return l, false
	}
	l.blobLen = size - l.blobStart
	return l, true
}

// docsValid checks every document has terms, its text inside the blob, and that the lengths sum to the header's.
func (s segView) docsValid(l segLayout) bool {
	var sumdl uint64
	for i := range l.ndocs {
		off := segHeader + i*docStride
		dl := s.u32(off)
		if dl == 0 || s.u32(off+4)+s.u32(off+8) > l.blobLen {
			return false
		}
		sumdl += dl
	}
	return sumdl == s.sumDL()
}

// termsValid checks every term's blob and posting ranges and that terms are strictly ascending.
func (s segView) termsValid(l segLayout) bool {
	var previous []byte
	for i := range l.nterms {
		off := l.termStart + 4 + i*16
		start, length := s.u32(off), s.u32(off+4)
		postOff, count := s.u32(off+8), s.u32(off+12)
		if start+length > l.blobLen || postOff%8 != 0 || postOff+count*8 > l.nposts*8 {
			return false
		}
		term := s[l.blobStart+start : l.blobStart+start+length]
		if i > 0 && bytes.Compare(term, previous) <= 0 {
			return false
		}
		previous = term
	}
	return true
}

// postsValid checks every posting names an existing document with a non-zero frequency.
func (s segView) postsValid(l segLayout) bool {
	for i := range l.nposts {
		off := l.postStart + 4 + i*8
		if s.u32(off) >= l.ndocs || s.u32(off+4) == 0 {
			return false
		}
	}
	return true
}

// compoundMinLen guards compound matching against over-matching very short terms as substrings.
const compoundMinLen = 4

// compoundWeight discounts a term matched only inside a longer token (agent -> subagent), so exact hits outrank.
const compoundWeight = 0.4

// lookupWeighted merges exact postings (weight 1) with compound ones (compoundWeight), deduped per doc by the stronger.
func (s segView) lookupWeighted(term string) map[int]float64 {
	out := map[int]float64{}
	add := func(t string, weight float64) {
		posts, n := s.lookup(t)
		for p := range n {
			doc := int(binary.LittleEndian.Uint32(posts[p*8:]))
			tf := float64(binary.LittleEndian.Uint32(posts[p*8+4:])) * weight
			if cur, ok := out[doc]; !ok || tf > cur {
				out[doc] = tf
			}
		}
	}
	add(term, 1)
	if len(term) >= compoundMinLen {
		for _, other := range s.containing(term) {
			add(other, compoundWeight)
		}
	}
	return out
}

// containing returns the indexed terms longer than term that contain it, in stored (sorted) order.
func (s segView) containing(term string) []string {
	termStart := segHeader + s.Docs()*docStride
	nterms := int(binary.LittleEndian.Uint32(s[termStart:]))
	if nterms == 0 || term == "" {
		return nil
	}
	postStart := termStart + 4 + nterms*16
	blobStart := postStart + 4 + int(binary.LittleEndian.Uint32(s[postStart:]))*8
	at := func(i int) (start, n int) { // absolute start and length of term i
		off := termStart + 4 + i*16
		return blobStart + int(binary.LittleEndian.Uint32(s[off:])), int(binary.LittleEndian.Uint32(s[off+4:]))
	}
	last, ln := at(nterms - 1)
	region := s[blobStart : last+ln] // the terms are stored contiguously, so one scan covers them all
	var out []string
	for pos := 0; pos < len(region); {
		h := bytes.Index(region[pos:], []byte(term))
		if h < 0 {
			break
		}
		h += blobStart + pos
		i := sort.Search(nterms, func(j int) bool { st, _ := at(j); return st > h }) - 1
		if i < 0 {
			break
		}
		st, n := at(i)
		if n > len(term) && h+len(term) <= st+n {
			out = append(out, string(s[st:st+n]))
		}
		if next := st + n - blobStart; next > pos { // a damaged file whose terms are not contiguous must not loop
			pos = next
		} else {
			break
		}
	}
	return out
}

// uniqTermIndex maps each distinct query term to a stable 0-based index.
func uniqTermIndex(qTerms []string) map[string]int {
	uniq := map[string]int{}
	for _, t := range qTerms {
		if _, ok := uniq[t]; !ok {
			uniq[t] = len(uniq)
		}
	}
	return uniq
}

// indexSegments computes per-term document frequency, per-segment per-doc weighted tf, and corpus stats.
func indexSegments(uniq map[string]int, segs []segView) (
	df []int, weighted []map[int][]float64, usable []int, ndocs int, avgdl float64,
) {
	df = make([]int, len(uniq))
	weighted = make([]map[int][]float64, len(segs)) // seg index -> doc -> per-term weighted tf
	var sumdl uint64
	for i, seg := range segs {
		if seg == nil {
			continue
		}
		usable = append(usable, i)
		ndocs += seg.Docs()
		sumdl += seg.sumDL()
		wt := map[int][]float64{}
		for term, j := range uniq {
			docs := seg.lookupWeighted(term)
			df[j] += len(docs)
			for doc, tf := range docs {
				if wt[doc] == nil {
					wt[doc] = make([]float64, len(uniq))
				}
				wt[doc][j] = tf
			}
		}
		weighted[i] = wt
	}
	if ndocs > 0 {
		avgdl = float64(sumdl) / float64(ndocs)
	}
	return df, weighted, usable, ndocs, avgdl
}

// scoreSegmentDocs returns the BM25F-scored hits for every matched doc in one segment.
func scoreSegmentDocs(qTerms []string, uniq map[string]int, df []int, n, avgdl float64,
	seg segView, wt map[int][]float64, file corpusFile, dir string) []SearchHit {
	var hits []SearchHit
	for doc := 0; doc < seg.Docs(); doc++ {
		freqs := wt[doc]
		if freqs == nil {
			continue
		}
		block, length := seg.Doc(doc)
		sum, id := seg.DocRef(doc)
		dl := float64(length)
		var score float64
		for _, t := range qTerms {
			j := uniq[t]
			tf := freqs[j]
			if tf == 0 {
				continue
			}
			idf := math.Log((n-float64(df[j])+0.5)/(float64(df[j])+0.5) + 1)
			score += idf * tf * (searchK1 + 1) / (tf + searchK1*(1-searchB+searchB*dl/avgdl))
		}
		if score <= 0 {
			continue
		}
		matched := 0
		for _, tf := range freqs {
			if tf > 0 {
				matched++
			}
		}
		hits = append(hits, SearchHit{
			Score: score, Matched: matched, Bank: file.Bank,
			File: filepath.Base(file.Path), Block: block, ID: id, Dir: dir, sum: sum,
		})
	}
	return hits
}

// sortByScore is hits stably ordered by descending score; it sorts an index permutation, as the hits are wide.
func sortByScore(hits []SearchHit) []SearchHit {
	if len(hits) < 2 {
		return hits
	}
	idx := make([]int32, len(hits))
	for i := range idx {
		idx[i] = int32(i)
	}
	slices.SortStableFunc(idx, func(a, b int32) int { // only "< 0" is read: the old `>` less, NaN included
		if hits[a].Score > hits[b].Score {
			return -1
		}
		return 0
	})
	out := make([]SearchHit, len(hits))
	for i, j := range idx {
		out[i] = hits[j]
	}
	return out
}

func rankFiles(qTerms []string, k int, files []corpusFile, segs []segView) []SearchHit {
	if len(qTerms) == 0 {
		return nil
	}
	uniq := uniqTermIndex(qTerms)
	df, weighted, usable, ndocs, avgdl := indexSegments(uniq, segs)
	if ndocs == 0 {
		return nil
	}
	n := float64(ndocs)
	var hits []SearchHit
	dir := accessDir()
	for _, i := range usable {
		hits = append(hits, scoreSegmentDocs(qTerms, uniq, df, n, avgdl, segs[i], weighted[i], files[i], dir)...)
	}
	hits = sortByScore(hits)
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// reservedNames is every filename a memory dir reserves; the six typed bank files are reserved by name too.
var reservedNames = reservedNameSet()

func reservedNameSet() map[string]bool {
	out := map[string]bool{
		nativeIndexFile:        true,
		nativeSearchableFile:   true,
		nativeArchiveIndexFile: true,
		catalogFile:            true,
		routerFile:             true,
	}
	for _, name := range bankFiles {
		out[name] = true
	}
	return out
}

// frontmatterValue is key's value inside a closed leading `---` block; "" when absent, unclosed, or a list/map.
func frontmatterValue(raw, key string) string {
	lines := strings.Split(raw, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	value := ""
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			return value
		}
		if v, found := strings.CutPrefix(line, key+":"); found && value == "" {
			value = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// readIfExists reads path; an absent file is empty, any other failure is returned. A var so tests count reads.
var readIfExists = func(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// Native auto-memory file names recognised when classifying a bank directory's files.
const (
	nativeIndexFile        = "MEMORY.md"     // the injected index of `- [Title](topic.md) — hook` lines
	nativeSearchableFile   = "SEARCHABLE.md" // legacy overflow file, classified only
	nativeArchiveIndexFile = "INDEX.md"      // legacy archive index, classified only
)

// topicFrontmatter splits a topic into name, description and body; the closing fence must start its own line.
func topicFrontmatter(raw string) (name, desc, body string, ok bool) {
	prefix, content := textutil.StripFrontMatter([]byte(raw))
	if prefix == nil {
		return "", "", "", false // an absent or unclosed block is body text, not frontmatter
	}
	for line := range strings.SplitSeq(string(prefix), "\n") {
		if v, found := strings.CutPrefix(line, "name:"); found {
			name = strings.Trim(strings.TrimSpace(v), `"'`)
		} else if v, found := strings.CutPrefix(line, "description:"); found {
			desc = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	if name == "" {
		return "", "", "", false
	}
	return name, desc, strings.TrimSpace(string(content)), true
}

// BM25 defaults; a block's first line is its title and weighs 3x.
const (
	searchK1          = 1.5
	searchB           = 0.75
	searchTitleWeight = 3
)

// stopwords are function words dropped from queries and blocks so paraphrases still rank.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "so": true, "of": true, "and": true, "or": true,
	"for": true, "in": true, "on": true, "at": true, "is": true, "are": true, "be": true, "it": true,
	"its": true, "this": true, "that": true, "with": true, "as": true, "by": true, "always": true,
	"just": true, "then": true, "than": true, "we": true, "you": true, "your": true,
	// Conversational filler: a prompt is prose, and one of these matching a block is not relevance.
	"i": true, "me": true, "my": true, "our": true, "us": true, "they": true, "them": true, "there": true,
	"here": true, "can": true, "do": true, "does": true, "did": true, "have": true, "has": true, "had": true,
	"how": true, "what": true, "which": true, "who": true, "why": true, "when": true, "where": true, "if": true,
	"but": true, "not": true, "no": true, "was": true, "were": true, "been": true, "will": true, "would": true,
	"could": true, "should": true, "some": true, "any": true, "all": true, "about": true, "from": true,
	"into": true, "over": true, "more": true, "less": true, "only": true, "also": true, "these": true,
	"those": true, "please": true, "want": true, "need": true, "make": true, "sure": true, "let": true,
}

// SearchHit is one matching bank block.
type SearchHit struct {
	UnresolvedReplacement bool    `json:"unresolvedReplacement,omitempty"`
	Score                 float64 `json:"score"`
	Matched               int     `json:"matched"` // distinct query terms the block contains
	Bank                  string  `json:"bank"`    // local | home | archive | native
	File                  string  `json:"file"`
	Block                 string  `json:"block"`
	ID                    string  `json:"id,omitempty"` // persisted block ID (`memory show <id>`)
	Dir                   string  `json:"-"`
	sum                   uint64  // blockSum(Block) when the segment supplied it, else 0; reset when Block changes
}

// key is blockKey(h.Block), taken from the segment's stored sum when there is one.
func (h SearchHit) key() string {
	if h.sum != 0 {
		return strconv.FormatUint(h.sum, 16)
	}
	return blockKey(h.Block)
}

// Title is the hit's first line, marker-stripped and truncated.
func (h SearchHit) Title() string {
	return truncateLine(strings.TrimLeft(stripID(textutil.FirstLine(h.Block)), "#- "))
}

func contentTerms(s string) []string {
	return contentQueryTerms(textutil.Tokenize(s))
}

// contentQueryTerms filters already tokenized query terms without changing their order or multiplicity.
func contentQueryTerms(tokens []string) []string {
	var out []string
	for _, t := range tokens {
		if !stopwords[t] {
			out = append(out, t)
		}
	}
	return out
}

// corpusBlocks loads every searchable block; a vanished file is skipped, any other read error fails the corpus.
func corpusBlocks() ([]SearchHit, error) {
	var out []SearchHit
	for _, f := range corpusOf() {
		data, err := os.ReadFile(f.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		base := filepath.Base(f.Path)
		dir := accessDir()
		for _, block := range parseBlocks(f.Mode, base, data) {
			out = append(out, SearchHit{Bank: f.Bank, File: base, Block: block, Dir: dir})
		}
	}
	return out, nil
}

// Entry is one discrete memory item (the ADK MemoryEntry shape): a bank block with id, date and type metadata.
type Entry struct {
	ID         string
	Date       string // YYYY-MM-DD from the title, "" when the block carries none
	Type       string // decision | convention | progress | session | context | topic | knowledge
	Title      string
	Body       string // the whole block, id markers stripped
	Bank, File string
	Archived   bool
	Superseded bool
}

var (
	entryDate = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	})
	// entryTypes maps a typed bank file to its entry type.
	entryTypes = map[string]string{
		"decisionLog.md": "decision", "conventions.md": "convention", "progress.md": "progress",
		"sessionHistory.md": "session", "activeContext.md": "context", "projectContext.md": "context",
	}
)

// stripIDMarks drops every `<!-- id:…-->` marker and the whitespace before it, in one linear scan.
func stripIDMarks(s string) string {
	const open = "<!-- id:"
	var b strings.Builder
	pos := 0
	for from := 0; ; {
		i := strings.Index(s[from:], open)
		if i < 0 {
			break
		}
		i += from
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			break
		}
		if end < len(open)+2 || s[i+end-2:i+end] != "--" {
			from = i + 1
			continue
		}
		b.WriteString(strings.TrimRight(s[pos:i], "\t\n\f\r "))
		pos = i + end + 1
		from = pos
	}
	if pos == 0 {
		return s
	}
	b.WriteString(s[pos:])
	return b.String()
}

// isSupersededTitle reports a fully struck-out or SUPERSEDED title line; a partly struck one is still live.
func isSupersededTitle(first string) bool {
	if strings.Contains(first, "SUPERSEDED") {
		return true
	}
	return strings.Contains(first, "~~") && !partlyStruck(first)
}

// ReplacedBy is the id of the entry that replaced h's block, "" when it is live or struck without a pointer.
func (h SearchHit) ReplacedBy() string { return replacedBy(h.Block) }

// entryFromHit lifts a search hit to an Entry; a block without a persisted id gets its contentRef.
func entryFromHit(h SearchHit) Entry {
	id := blockRef(h.Block, h.ID)
	typ := entryTypes[logicalName(h.File)]
	switch {
	case typ != "":
	case h.Bank == "knowledge":
		typ = "knowledge"
	default:
		typ = "topic"
	}
	title := strings.TrimLeft(stripIDMarks(textutil.FirstLine(h.Block)), "#- ")
	return Entry{
		ID: id, Date: entryDate().FindString(title), Type: typ, Title: truncateLine(title),
		Body: strings.TrimSpace(stripIDMarks(h.Block)), Bank: h.Bank, File: h.File,
		Archived: h.Bank == "archive", Superseded: isSupersededBlock(h.Block),
	}
}

// pointer is the one-line reference `memory search <id>` expands: id · date · title.
func (e Entry) pointer() string {
	date := e.Date
	if date == "" {
		date = "undated"
	}
	return e.ID + " · " + date + " · " + e.Title
}

// typeRank orders entry types for recall: decisions and conventions first, session narrative last.
func (e Entry) typeRank() int {
	switch e.Type {
	case "decision", "convention":
		return 0
	case "session":
		return 2
	}
	return 1
}
