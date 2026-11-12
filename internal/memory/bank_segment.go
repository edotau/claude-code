package memory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
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

// corpusFiles preserves bank, archive, native and knowledge order without reading file contents.
func corpusFiles(root string) []corpusFile {
	var out []corpusFile
	banks := func(bank, dir string) {
		for _, name := range bankFiles {
			out = append(out, corpusFile{bank, filepath.Join(dir, name), segEntries})
		}
	}
	if !isHomeBank(root) {
		banks("local", Dir(root))
	}
	banks("home", HomeBankDir())
	archiveOwners := []string{Dir(root)}
	if !isHomeBank(root) {
		archiveOwners = append(archiveOwners, HomeBankDir())
	}
	for _, owner := range archiveOwners {
		for _, path := range globIn(archiveDir(owner), "*.md") {
			out = append(out, corpusFile{"archive", path, segEntries})
		}
	}
	for _, dir := range nativeDirs(root) {
		for _, path := range searchTopicFiles(dir) {
			out = append(out, corpusFile{"native", path, segNative})
		}
	}
	for _, path := range knowledgeFiles(root) {
		out = append(out, corpusFile{"knowledge", path, segKnowledge})
	}
	return out
}

// ownerDir maps a corpus file back to the bank directory that owns its access log.
func ownerDir(bank, path string) string {
	if bank == "archive" || bank == "knowledge" {
		return filepath.Dir(filepath.Dir(path))
	}
	return filepath.Dir(path)
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
		if reservedKindFold(name) && !hasExactEntry(dir, name) {
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
	termStart := 12 + len(docs)*12
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
		off := 12 + i*12
		binary.LittleEndian.PutUint32(s[off:], d.dl)
		binary.LittleEndian.PutUint32(s[off+4:], uint32(len(s)-blobStart))
		binary.LittleEndian.PutUint32(s[off+8:], uint32(len(d.text)))
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
	termStart := 12 + s.Docs()*12
	postStart := termStart + 4 + int(binary.LittleEndian.Uint32(s[termStart:]))*16
	blobStart := postStart + 4 + int(binary.LittleEndian.Uint32(s[postStart:]))*8
	off := 12 + i*12
	start := blobStart + int(binary.LittleEndian.Uint32(s[off+4:]))
	end := start + int(binary.LittleEndian.Uint32(s[off+8:]))
	return string(s[start:end]), binary.LittleEndian.Uint32(s[off:])
}

// Posting offsets are byte offsets relative to the posting table; text and term offsets are blob-relative.
func (s segView) lookup(term string) (posts []byte, n int) {
	termStart := 12 + s.Docs()*12
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
	l.termStart = 12 + l.ndocs*12
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
		off := 12 + i*12
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
func (s segView) lookupWeighted(term string, all []string) map[int]float64 {
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
		for _, other := range all {
			if other != term && strings.Contains(other, term) {
				add(other, compoundWeight)
			}
		}
	}
	return out
}

// terms returns every indexed term in the segment, in their stored (sorted) order.
func (s segView) terms() []string {
	termStart := 12 + s.Docs()*12
	nterms := int(binary.LittleEndian.Uint32(s[termStart:]))
	postStart := termStart + 4 + nterms*16
	blobStart := postStart + 4 + int(binary.LittleEndian.Uint32(s[postStart:]))*8
	out := make([]string, nterms)
	for i := range nterms {
		off := termStart + 4 + i*16
		start := blobStart + int(binary.LittleEndian.Uint32(s[off:]))
		end := start + int(binary.LittleEndian.Uint32(s[off+4:]))
		out[i] = string(s[start:end])
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
		var all []string
		for term, j := range uniq {
			if all == nil && len(term) >= compoundMinLen {
				all = seg.terms()
			}
			docs := seg.lookupWeighted(term, all)
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
	seg segView, wt map[int][]float64, file corpusFile) []SearchHit {
	var hits []SearchHit
	for doc := 0; doc < seg.Docs(); doc++ {
		freqs := wt[doc]
		if freqs == nil {
			continue
		}
		block, length := seg.Doc(doc)
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
		id := blockRef(block, "")
		hits = append(hits, SearchHit{
			Score: score, Matched: matched, Bank: file.Bank,
			File: filepath.Base(file.Path), Block: block, ID: id, Dir: ownerDir(file.Bank, file.Path),
		})
	}
	return hits
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
	for _, i := range usable {
		hits = append(hits, scoreSegmentDocs(qTerms, uniq, df, n, avgdl, segs[i], weighted[i], files[i])...)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// routedFiles are the bank and topic files of every named repo, other than root's own store.
func routedFiles(root, query string) []corpusFile {
	own := Dir(root)
	var out []corpusFile
	for _, e := range routedRepos(query) {
		if e.Dir != own {
			out = append(out, storeFiles(e)...)
		}
	}
	return out
}

// storeFiles labels a repo's typed bank files and native topics with its store key.
func storeFiles(e repoEntry) []corpusFile {
	var out []corpusFile
	for _, name := range BankFiles() {
		out = append(out, corpusFile{Bank: e.Key, Path: filepath.Join(e.Dir, name), Mode: segEntries})
	}
	for _, path := range searchTopicFiles(e.Dir) {
		out = append(out, corpusFile{Bank: e.Key, Path: path, Mode: segNative})
	}
	return out
}

// reservedNames is every filename a memory dir reserves; the six typed bank files are reserved by name too.
var reservedNames = reservedNameSet()

func reservedNameSet() map[string]bool {
	out := map[string]bool{
		nativeIndexFile:        true,
		nativeSearchableFile:   true,
		nativeArchiveIndexFile: true,
		catalogFile:            true,
	}
	for _, name := range bankFiles {
		out[name] = true
	}
	return out
}

// reservedKindFold reports whether name is reserved under case folding ("activecontext.md" opens activeContext.md).
func reservedKindFold(name string) bool {
	for reserved := range reservedNames {
		if strings.EqualFold(reserved, name) {
			return true
		}
	}
	return false
}

// hasExactEntry reports whether dir lists name in exactly this spelling; false means Lstat hit a case-folded twin.
func hasExactEntry(dir, name string) bool {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() == name {
			return true
		}
	}
	return false
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

// nativeDirs lists Claude Code's auto-memory dirs recall searches: the harness's own (user-wide facts), then root's.
func nativeDirs(root string) []string {
	out := []string{HomeBankDir()} // native shares the repo's store dir; the home tier holds the user-wide notes
	if d := Dir(root); !slices.Contains(out, d) {
		out = append(out, d)
	}
	return out
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
