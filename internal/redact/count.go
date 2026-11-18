package redact

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"
	"regexp/syntax"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/textutil"
)

const CountWindow = 1 << 20

type countSpan struct{ start, end int64 }

type Counter struct {
	matcher  *Matcher
	prefixes map[uint32][]string
	starts   [256]bool
	pairs    [65536]bool
	initials string
	nonASCII bool
	buffers  sync.Pool
}

func NewCounter(m *Matcher) *Counter {
	c := &Counter{matcher: m, prefixes: make(map[uint32][]string)}
	for _, bucket := range m.needles {
		for _, needle := range bucket {
			key := countKey([]byte(needle))
			c.starts[needle[0]] = true
			c.pairs[uint16(needle[0])<<8|uint16(needle[1])] = true
			c.prefixes[key] = append(c.prefixes[key], needle)
		}
	}
	for i, start := range c.starts {
		if start {
			if i >= 128 {
				c.nonASCII = true
			} else {
				c.initials += string(byte(i))
			}
		}
	}
	c.buffers.New = func() any { b := make([]byte, CountWindow+max(m.longest, 16)); return &b }
	return c
}

func (c *Counter) Matcher() *Matcher { return c.matcher }

func countKey(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

func (c *Counter) width(b []byte) int {
	if len(b) < 3 || !c.starts[b[0]] || !c.pairs[uint16(b[0])<<8|uint16(b[1])] {
		return 0
	}
	for _, needle := range c.prefixes[countKey(b)] {
		if bytes.HasPrefix(b, []byte(needle)) {
			return len(needle)
		}
	}
	return 0
}

func (c *Counter) File(path string) (hits int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, errors.New("redact: count open failed")
	}
	defer func() {
		if f.Close() != nil {
			err = errors.New("redact: count close failed")
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return 0, errors.New("redact: count stat failed")
	}
	bp := c.buffers.Get().(*[]byte)
	defer c.buffers.Put(bp)
	spans, masks, shapes, err := c.values(f, info.Size(), *bp)
	if err != nil {
		return 0, err
	}
	hits = len(spans)
	hidden := mergeCountSpans(append(spans, masks...))
	if shapes {
		var count int
		count, err = countShapes(f, info.Size(), hidden, (*bp)[:CountWindow])
		if err != nil {
			return 0, err
		}
		hits += count
	}
	current, err := f.Stat()
	if err != nil || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return 0, errors.New("redact: file changed during count")
	}
	return hits, nil
}

// Three-byte buckets reject values; complete shape prefixes reject shape passes, never individual samples.
func (c *Counter) values(f *os.File, size int64, buf []byte) ([]countSpan, []countSpan, bool, error) {
	overlap := max(c.matcher.longest, 16)
	var spans, masks []countSpan
	var maskEnd int64
	shapes := false
	for offset := int64(0); offset < size; {
		n, err := f.ReadAt(buf[:min(int64(len(buf)), size-offset)], offset)
		if err != nil && err != io.EOF {
			return nil, nil, false, errors.New("redact: count read failed")
		}
		b := buf[:n]
		shapes = shapes || possibleShape(b)
		limit := n
		if offset+int64(n) < size {
			limit -= overlap
		}
		if limit <= 0 {
			return nil, nil, false, errors.New("redact: count made no progress")
		}
		spans, masks, maskEnd = c.valueWindow(b, limit, offset, maskEnd, spans, masks)
		offset += int64(limit)
	}
	return spans, masks, shapes, nil
}

func (c *Counter) valueWindow(b []byte, limit int, offset, maskEnd int64, spans, masks []countSpan) ([]countSpan, []countSpan, int64) {
	if !c.nonASCII && (len(masks) == 0 || offset > maskEnd) && !bytes.ContainsAny(b, "[*") && !bytes.ContainsAny(b, c.initials) {
		return spans, masks, maskEnd
	}
	for i := range limit {
		if b[i] != '[' && b[i] != '*' && offset+int64(i) > maskEnd && !c.starts[b[i]] {
			continue
		}
		pos := offset + int64(i)
		if pos < maskEnd {
			continue
		}
		if pos == maskEnd && len(masks) > 0 && b[i] == '*' {
			maskEnd++
			masks[len(masks)-1].end = maskEnd
			continue
		}
		if (len(spans) == 0 || pos >= spans[len(spans)-1].end) && bytes.HasPrefix(b[i:], []byte(marker)) {
			maskEnd = pos + int64(len(marker))
			masks = append(masks, countSpan{pos, maskEnd})
			continue
		}
		if b[i] == '*' && len(spans) > 0 {
			masks = countMaskTail(masks, pos, spans[len(spans)-1].end)
		}
		if width := c.width(b[i:]); width > 0 {
			span := countSpan{pos, pos + int64(width)}
			if len(spans) > 0 && spans[len(spans)-1].end > pos {
				spans[len(spans)-1].end = max(spans[len(spans)-1].end, span.end)
			} else {
				spans = append(spans, span)
			}
		}
	}
	return spans, masks, maskEnd
}

func countMaskTail(masks []countSpan, pos, end int64) []countSpan {
	if len(masks) > 0 && masks[len(masks)-1].end == pos && masks[len(masks)-1].start >= end {
		masks[len(masks)-1].end++
	} else if pos == end {
		masks = append(masks, countSpan{pos, pos + 1})
	}
	return masks
}

func possibleShape(b []byte) bool {
	for _, prefix := range countShapePrefixes() {
		if bytes.Contains(b, []byte(prefix)) {
			return true
		}
	}
	return countCandidate(b, 2) >= 0
}

// Deriving prefixes from the shape regex keeps the rejection filter sound as token families change.
var countShapePrefixes = sync.OnceValue(func() []string {
	pattern, err := syntax.Parse(textutil.CredentialRE().String(), syntax.Perl)
	if err != nil {
		panic("invalid credential pattern")
	}
	prefixes, _ := countLiteralPrefixes(pattern)
	return prefixes
})

func countLiteralPrefixes(r *syntax.Regexp) ([]string, bool) {
	switch r.Op {
	case syntax.OpLiteral:
		if r.Flags&syntax.FoldCase != 0 {
			return []string{""}, false
		}
		return []string{string(r.Rune)}, true
	case syntax.OpCapture:
		return countLiteralPrefixes(r.Sub[0])
	case syntax.OpConcat:
		out := []string{""}
		for _, sub := range r.Sub {
			prefixes, complete := countLiteralPrefixes(sub)
			out = countJoinPrefixes(out, prefixes)
			if !complete {
				return out, false
			}
		}
		return out, true
	case syntax.OpAlternate:
		var out []string
		complete := true
		for _, sub := range r.Sub {
			prefixes, literal := countLiteralPrefixes(sub)
			complete = complete && literal
			out = append(out, prefixes...)
		}
		return out, complete
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return []string{""}, true
	default:
		return []string{""}, false
	}
}

func countJoinPrefixes(left, right []string) []string {
	out := make([]string, 0, len(left)*len(right))
	for _, a := range left {
		for _, b := range right {
			out = append(out, a+b)
		}
	}
	return out
}

var countPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)^-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`),
	regexp.MustCompile(`^eyJ[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}(?:\.[A-Za-z0-9_-]*)?`),
	regexp.MustCompile(`(?i)^(bearer\s+)\S{40,}`),
}

func countShapes(f *os.File, size int64, hidden []countSpan, buf []byte) (int, error) {
	hits := 0
	for stage := range countPatterns {
		added, err := countStage(f, size, hidden, stage, buf)
		if err != nil {
			return 0, err
		}
		hits += len(added)
		hidden = mergeCountSpans(append(hidden, added...))
	}
	runs, err := countRuns(f, size, hidden, buf)
	return hits + runs, err
}

func countStage(f *os.File, size int64, hidden []countSpan, stage int, buf []byte) ([]countSpan, error) {
	var added []countSpan
	for offset := int64(0); offset < size; {
		r := &countReader{f: f, offset: offset, size: size, hidden: hidden}
		n, err := r.Read(buf)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if n == 0 {
			break
		}
		advance := max(1, n-16)
		for cursor := 0; cursor < advance; {
			index := countCandidate(buf[cursor:n], stage)
			if index < 0 {
				break
			}
			index += cursor
			start := offset + int64(index)
			loc, err := countAt(f, size, hidden, stage, start)
			if err != nil {
				return nil, err
			}
			if loc == nil {
				cursor = index + 1
				continue
			}
			end := start + int64(loc[1])
			if stage == 2 {
				start += int64(loc[3])
			}
			added = append(added, countSpan{start, end})
			cursor = int(end - offset)
			advance = max(advance, cursor)
		}
		offset += int64(advance)
	}
	return added, nil
}

func countCandidate(b []byte, stage int) int {
	if stage < 2 {
		return bytes.Index(b, []byte([]string{"-----BEGIN ", "eyJ"}[stage]))
	}
	for offset := 0; offset+6 <= len(b); {
		index := bytes.IndexAny(b[offset:], "bB")
		if index < 0 {
			return -1
		}
		index += offset
		if index+6 <= len(b) && strings.EqualFold(string(b[index:index+6]), "bearer") {
			return index
		}
		offset = index + 1
	}
	return -1
}

func countAt(f *os.File, size int64, hidden []countSpan, stage int, offset int64) ([]int, error) {
	if stage == 1 && offset > 0 {
		var prev [1]byte
		r := &countReader{f: f, offset: offset - 1, size: size, hidden: hidden}
		n, err := r.Read(prev[:])
		if err != nil && err != io.EOF {
			return nil, err
		}
		if n == 1 && countWord(prev[0]) {
			return nil, nil
		}
	}
	r := &countReader{f: f, offset: offset, size: size, hidden: hidden}
	loc := countPatterns[stage].FindReaderSubmatchIndex(bufio.NewReaderSize(r, 4096))
	return loc, r.err
}

func countWord(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_'
}

func countTokenCandidate(b []byte) bool {
	for _, prefix := range countShapePrefixes() {
		if bytes.HasPrefix(b, []byte(prefix)) {
			return true
		}
	}
	return false
}

func countRuns(f *os.File, size int64, hidden []countSpan, buf []byte) (int, error) {
	r := &countReader{f: f, size: size, hidden: hidden}
	var prefix [128]byte
	length, hits := 0, 0
	finish := func() {
		if length >= 4 && countTokenCandidate(prefix[:min(length, len(prefix))]) && textutil.LooksSecret(string(prefix[:min(length, len(prefix))])) {
			hits++
		}
		length = 0
	}
	for {
		n, err := r.Read(buf)
		for _, ch := range buf[:n] {
			if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' {
				if length < len(prefix) {
					prefix[length] = ch
				}
				length++
			} else {
				finish()
			}
		}
		if err != nil {
			break
		}
	}
	finish()
	return hits, r.err
}

type countReader struct {
	f            *os.File
	offset, size int64
	hidden       []countSpan
	err          error
}

func (r *countReader) Read(b []byte) (int, error) {
	if r.offset >= r.size {
		return 0, io.EOF
	}
	b = b[:min(int64(len(b)), r.size-r.offset)]
	n, err := r.f.ReadAt(b, r.offset)
	if err != nil && err != io.EOF {
		r.err = errors.New("redact: count read failed")
		return n, r.err
	}
	first := sort.Search(len(r.hidden), func(i int) bool { return r.hidden[i].end > r.offset })
	if first < len(r.hidden) {
		for _, span := range r.hidden[first:] {
			if span.start >= r.offset+int64(n) {
				break
			}
			for i := max(span.start, r.offset); i < min(span.end, r.offset+int64(n)); i++ {
				b[i-r.offset] = ' '
			}
		}
	}
	r.offset += int64(n)
	return n, err
}

func mergeCountSpans(spans []countSpan) []countSpan {
	slices.SortFunc(spans, func(a, b countSpan) int {
		if a.start < b.start {
			return -1
		}
		if a.start > b.start {
			return 1
		}
		return 0
	})
	out := spans[:0]
	for _, span := range spans {
		if len(out) > 0 && span.start < out[len(out)-1].end {
			out[len(out)-1].end = max(out[len(out)-1].end, span.end)
		} else {
			out = append(out, span)
		}
	}
	return out
}
