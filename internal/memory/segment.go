package memory

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type segMode uint8

const (
	segEntries segMode = iota
	segNative
	segKnowledge
)

type corpusFile struct {
	bank, path string
	mode       segMode
}

// corpusFiles preserves bank, archive, native and knowledge order without reading file contents.
func corpusFiles(root string) []corpusFile {
	var out []corpusFile
	banks := func(bank, dir string) {
		for _, name := range bankFiles {
			out = append(out, corpusFile{bank, filepath.Join(dir, name), segEntries})
		}
	}
	if !IsHomeBank(root) {
		banks("local", Dir(root))
	}
	banks("home", HomeBankDir())
	archived, _ := filepath.Glob(filepath.Join(Dir(root), "archive", "*.md"))
	for _, path := range archived {
		out = append(out, corpusFile{"archive", path, segEntries})
	}
	for _, dir := range NativeDirs(root) {
		for _, path := range searchTopicFiles(dir) {
			out = append(out, corpusFile{"native", path, segNative})
		}
	}
	if CutOver() {
		for _, path := range knowledgeFiles(filepath.Base(Dir(root))) {
			out = append(out, corpusFile{"knowledge", path, segKnowledge})
		}
	}
	return out
}

func searchTopicFiles(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var files []string
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if _, ok := reservedNames[name]; ok {
			continue
		}
		if _, ok := reservedKindFold(name); ok && !hasExactEntry(dir, name) {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files
}

func parseBlocks(mode segMode, base string, data []byte) []string {
	s := string(data)
	switch mode {
	case segEntries:
		_, blocks := entryBlocks(base, s)
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

func encodeSegment(blocks []string) segView {
	type document struct {
		text string
		dl   uint32
	}
	type posting struct {
		doc, tf uint32
	}
	var docs []document
	postings := map[string][]posting{}
	var sumdl uint64
	nposts := 0
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
			postings[term] = append(postings[term], posting{uint32(len(docs)), count})
			nposts++
		}
		docs = append(docs, document{block, dl})
		sumdl += uint64(dl)
	}
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

func (s segView) docs() int {
	return int(binary.LittleEndian.Uint32(s))
}

func (s segView) sumDL() uint64 {
	return binary.LittleEndian.Uint64(s[4:])
}

func (s segView) doc(i int) (text string, dl uint32) {
	termStart := 12 + s.docs()*12
	postStart := termStart + 4 + int(binary.LittleEndian.Uint32(s[termStart:]))*16
	blobStart := postStart + 4 + int(binary.LittleEndian.Uint32(s[postStart:]))*8
	off := 12 + i*12
	start := blobStart + int(binary.LittleEndian.Uint32(s[off+4:]))
	end := start + int(binary.LittleEndian.Uint32(s[off+8:]))
	return string(s[start:end]), binary.LittleEndian.Uint32(s[off:])
}

// Posting offsets are byte offsets relative to the posting table; text and term offsets are blob-relative.
func (s segView) lookup(term string) (posts []byte, n int) {
	termStart := 12 + s.docs()*12
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

func (s segView) valid() bool {
	size := uint64(len(s))
	if size < 12 {
		return false
	}
	u32 := func(off uint64) uint64 { return uint64(binary.LittleEndian.Uint32(s[off:])) }
	ndocs := u32(0)
	termStart := 12 + ndocs*12
	if termStart+4 > size {
		return false
	}
	nterms := u32(termStart)
	postStart := termStart + 4 + nterms*16
	if postStart+4 > size {
		return false
	}
	nposts := u32(postStart)
	blobStart := postStart + 4 + nposts*8
	if blobStart > size {
		return false
	}
	blobLen := size - blobStart
	var sumdl uint64
	for i := uint64(0); i < ndocs; i++ {
		off := 12 + i*12
		dl := u32(off)
		if dl == 0 || u32(off+4)+u32(off+8) > blobLen {
			return false
		}
		sumdl += dl
	}
	if sumdl != s.sumDL() {
		return false
	}
	var previous []byte
	for i := uint64(0); i < nterms; i++ {
		off := termStart + 4 + i*16
		start, length := u32(off), u32(off+4)
		postOff, count := u32(off+8), u32(off+12)
		if start+length > blobLen || postOff%8 != 0 || postOff+count*8 > nposts*8 {
			return false
		}
		term := s[blobStart+start : blobStart+start+length]
		if i > 0 && bytes.Compare(term, previous) <= 0 {
			return false
		}
		previous = term
	}
	for i := uint64(0); i < nposts; i++ {
		off := postStart + 4 + i*8
		if u32(off) >= ndocs || u32(off+4) == 0 {
			return false
		}
	}
	return true
}

// compoundMinLen guards compound matching against over-matching very short terms as substrings.
const compoundMinLen = 4

// compoundWeight discounts a query term matched only as part of a longer indexed token (agent -> subagent),
// so an exact hit always outranks a compound one.
const compoundWeight = 0.4

// lookupWeighted merges exact postings (weight 1) with postings of every other indexed token containing
// term (weight compoundWeight), deduped per doc by the stronger signal.
func (s segView) lookupWeighted(term string) map[int]float64 {
	out := map[int]float64{}
	add := func(t string, weight float64) {
		posts, n := s.lookup(t)
		for p := 0; p < n; p++ {
			doc := int(binary.LittleEndian.Uint32(posts[p*8:]))
			tf := float64(binary.LittleEndian.Uint32(posts[p*8+4:])) * weight
			if cur, ok := out[doc]; !ok || tf > cur {
				out[doc] = tf
			}
		}
	}
	add(term, 1)
	if len(term) >= compoundMinLen {
		for _, other := range s.terms() {
			if other != term && strings.Contains(other, term) {
				add(other, compoundWeight)
			}
		}
	}
	return out
}

// terms returns every indexed term in the segment, in their stored (sorted) order.
func (s segView) terms() []string {
	termStart := 12 + s.docs()*12
	nterms := int(binary.LittleEndian.Uint32(s[termStart:]))
	postStart := termStart + 4 + nterms*16
	blobStart := postStart + 4 + int(binary.LittleEndian.Uint32(s[postStart:]))*8
	out := make([]string, nterms)
	for i := 0; i < nterms; i++ {
		off := termStart + 4 + i*16
		start := blobStart + int(binary.LittleEndian.Uint32(s[off:]))
		end := start + int(binary.LittleEndian.Uint32(s[off+4:]))
		out[i] = string(s[start:end])
	}
	return out
}

func rankFiles(qTerms []string, k int, files []corpusFile, segs []segView) []SearchHit {
	if len(qTerms) == 0 {
		return nil
	}
	uniq := map[string]int{}
	for _, t := range qTerms {
		if _, ok := uniq[t]; !ok {
			uniq[t] = len(uniq)
		}
	}
	df := make([]int, len(uniq))
	var ndocs int
	var sumdl uint64
	var usable []int
	weighted := make([]map[int][]float64, len(segs)) // seg index -> doc -> per-term weighted tf
	for i, seg := range segs {
		if seg == nil {
			continue
		}
		usable = append(usable, i)
		ndocs += seg.docs()
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
	if ndocs == 0 {
		return nil
	}
	n := float64(ndocs)
	avgdl := float64(sumdl) / float64(ndocs)
	var hits []SearchHit
	for _, i := range usable {
		seg := segs[i]
		wt := weighted[i]
		for doc := 0; doc < seg.docs(); doc++ {
			freqs := wt[doc]
			if freqs == nil {
				continue
			}
			block, length := seg.doc(doc)
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
			if score > 0 {
				matched := 0
				for _, tf := range freqs {
					if tf > 0 {
						matched++
					}
				}
				first, _, _ := strings.Cut(block, "\n")
				id := BlockID(first)
				if id == "" {
					id = ContentRef(block)
				}
				hits = append(hits, SearchHit{
					Score: score, Matched: matched, Bank: files[i].bank,
					File: filepath.Base(files[i].path), Block: block, ID: id,
				})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}
