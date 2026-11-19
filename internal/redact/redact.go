package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// MinLen excludes short values that commonly occur in ordinary prose.
const MinLen = 12
const marker = "[redacted]"
const windowSize = 4 << 20

// Matcher keeps the value index reusable across independent text buffers.
type Matcher struct {
	needles [256][]string
	origins map[string]Origin
	Skipped int
	longest int
	breaks  bool // some value holds a line break, so a match can span lines
}

// Origin names a needle without its bytes: vault record, JSON field, encoded form and length.
type Origin struct {
	Record, Field, Form string
	Len                 int
}

type sourceError struct{ cause error }

func (e sourceError) Error() string { return "redact: value source unavailable" }
func (e sourceError) Unwrap() error { return e.cause }

// Load accepts a vault adapter without coupling this package to vault storage or its errors.
func Load(values func() ([]string, error)) (*Matcher, error) {
	vals, err := values()
	if err != nil {
		return nil, sourceError{err}
	}
	return FromValues(vals), nil
}

func FromValues(vals []string) *Matcher {
	recs := make([][2]string, len(vals))
	for i, v := range vals {
		recs[i][1] = v
	}
	return fromRecords(recs)
}

// FromRecords is FromValues keeping each value's record name, so a Tally can attribute matches.
func FromRecords(recs map[string]string) *Matcher {
	pairs := make([][2]string, 0, len(recs))
	for name, v := range recs {
		pairs = append(pairs, [2]string{name, v})
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a][0] < pairs[b][0] })
	return fromRecords(pairs)
}

func fromRecords(recs [][2]string) *Matcher {
	m := &Matcher{origins: map[string]Origin{}}
	for _, rec := range recs {
		for _, leaf := range leaves(rec[1]) {
			if !eligible(leaf.text) {
				m.Skipped++
				continue
			}
			for i, v := range variants(leaf.text) {
				if _, dup := m.origins[v]; !dup {
					m.needles[v[0]] = append(m.needles[v[0]], v)
					m.origins[v] = Origin{rec[0], leaf.field, variantForms[i], len(v)}
					m.longest = max(m.longest, len(v))
					m.breaks = m.breaks || strings.Contains(v, "\n")
				}
			}
		}
	}
	for i := range m.needles {
		sort.Slice(m.needles[i], func(a, b int) bool { return len(m.needles[i][a]) > len(m.needles[i][b]) })
	}
	return m
}

// Values is how many distinct values the matcher masks, each in all its encoded forms.
func (m *Matcher) Values() int {
	n := 0
	for _, o := range m.origins {
		if o.Form == "raw" {
			n++
		}
	}
	return n
}

type leaf struct{ field, text string }

func leaves(val string) []leaf {
	var obj any
	if json.Unmarshal([]byte(val), &obj) != nil {
		return []leaf{{"", val}}
	}
	switch obj.(type) {
	case map[string]any, []any:
	default:
		return []leaf{{"", val}}
	}
	var out []leaf
	var visit func(string, any)
	visit = func(field string, v any) {
		switch x := v.(type) {
		case string:
			if !strings.Contains(x, "://") { // a URL leaf is an address, never the credential
				out = append(out, leaf{field, x})
			}
		case []any:
			for _, item := range x {
				visit(field+"[]", item)
			}
		case map[string]any:
			for k, item := range x {
				visit(strings.TrimPrefix(field+"."+k, "."), item)
			}
		}
	}
	visit("", obj)
	return out
}

func eligible(v string) bool {
	if len(v) < MinLen || textutil.HasAnyPrefix(v, "$", "/", "~/", "@secret") {
		return false
	}
	var classes uint
	for _, c := range []byte(v) {
		switch {
		case c >= 'a' && c <= 'z':
			classes |= 1
		case c >= 'A' && c <= 'Z':
			classes |= 2
		case c >= '0' && c <= '9':
			classes |= 4
		default:
			classes |= 8
		}
	}
	return bits.OnesCount(classes) >= 2
}

var variantForms = []string{"raw", "json", "query", "bearer", "base64", "base64-raw", "base64url", "base64url-raw"}

func variants(v string) []string {
	encoded, _ := json.Marshal(v)
	out := []string{v, string(encoded[1 : len(encoded)-1]), url.QueryEscape(v), "Bearer " + v}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		out = append(out, enc.EncodeToString([]byte(v)))
	}
	return out
}

func fill(b []byte) {
	for i := range b {
		b[i] = '*'
	}
	if len(b) > len(marker) {
		copy(b, marker)
	}
}

func (m *Matcher) Mask(b []byte) int {
	hits, _ := m.mask(b, nil)
	return hits
}

// mask also reports a shape that could run past the end of b: an open PEM block or a bearer still awaiting its token.
func (m *Matcher) mask(b []byte, log *[]hit) (hits int, open bool) {
	hits, _ = m.scan(b, true, nil, log)
	text := shapeText(b)
	spans := textutil.SecretSpans(text)
	for _, span := range spans {
		if log != nil {
			*log = append(*log, hit{kind: shapeKind(text, span)})
		}
		fill(b[span[0]:span[1]])
		hits++
	}
	tail := strings.TrimRight(text, "\t\n\f\r ")
	bearer := len(tail) >= len("bearer") && strings.EqualFold(tail[len(tail)-len("bearer"):], "bearer")
	return hits, bearer || len(spans) > 0 && spans[len(spans)-1][1] == len(b)
}

// MaskValues masks only exact vault values and their encodings, for text whose structure a shape span could cut.
func (m *Matcher) MaskValues(b []byte) int {
	hits, _ := m.scan(b, true, nil, nil)
	return hits
}

// scan masks the leading bytes of b whose masking no later byte can change (all of b when final) and counts them.
func (m *Matcher) scan(b []byte, final bool, lx *jsonLexer, log *[]hit) (hits, done int) {
	for done < len(b) {
		if c := b[done]; c != '[' && len(m.needles[c]) == 0 { // neither a mask nor a value starts here
			if lx != nil {
				lx.advance(c)
			}
			done++
			continue
		}
		width, hit, ok := m.step(b[done:], final)
		if !ok {
			break
		}
		span := b[done : done+width]
		if log != nil {
			m.note(log, span, hit) // before the switch masks span
		}
		switch {
		case hit && lx == nil:
			fill(span)
		case hit:
			lx.mask(span)
		case lx != nil:
			lx.skip(span)
		}
		if hit {
			hits++
		}
		done += width
	}
	return hits, done
}

// step is one move of the left-to-right scan at b[0]: an existing mask, a value span with every overlap, or one byte.
func (m *Matcher) step(b []byte, final bool) (width int, hit, ok bool) {
	if n, ok := maskWidth(b, final); n > 0 || !ok {
		return n, false, ok
	}
	end, ok := m.matchAt(b, final)
	if !ok || end == 0 {
		return 1, false, ok
	}
	for j := 1; j < end; j++ {
		n, ok := m.matchAt(b[j:], final)
		if !ok {
			return 0, false, false
		}
		end = max(end, j+n)
	}
	return end, true, true
}

// matchAt is the longest value at b[0]; not ok while b is a proper prefix of a longer value that could still complete.
func (m *Matcher) matchAt(b []byte, final bool) (int, bool) {
	for _, needle := range m.needles[b[0]] {
		if len(needle) > len(b) {
			if !final && needle[:len(b)] == string(b) {
				return 0, false
			}
			continue
		}
		if needle[1] == b[1] && string(b[:len(needle)]) == needle { // MinLen guarantees a second byte
			return len(needle), true
		}
	}
	return 0, true
}

func (m *Matcher) String(s string) string {
	b := []byte(s)
	if m.Mask(b) == 0 {
		return s
	}
	return string(b)
}

// File masks path in place; links leading to root are the owner's layout, any link below root is refused.
func File(root, path string, m *Matcher, idle time.Duration) (int, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 0, errors.New("redact: path outside root")
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return 0, errors.New("redact: root open failed")
	}
	defer dir.Close()
	return FileAt(dir, rel, m, idle)
}

// FileAt is File for name under root; a root opened before a wait keeps a later directory swap from moving the write.
func FileAt(root *os.Root, name string, m *Matcher, idle time.Duration) (hits int, err error) {
	f, info, err := openNoLinks(root, name)
	if err != nil {
		return 0, err
	}
	defer func() {
		if f.Close() != nil {
			err = errors.New("redact: file close failed")
		}
	}()
	if time.Since(info.ModTime()) < idle {
		return 0, nil
	}
	before, err := snapshot(f, info.Size(), max(0, m.longest-1))
	if err != nil {
		return 0, err
	}
	after := bytes.Clone(before)
	hits = m.Mask(after)
	if hits == 0 {
		return 0, nil
	}
	if filepath.Ext(name) == ".jsonl" && !validLines(before, after) {
		return 0, errors.New("redact: invalid changed JSON line")
	}
	current, err := f.Stat()
	if err != nil || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return 0, errors.New("redact: file changed during scan")
	}
	if err := writeChanges(f, before, after); err != nil {
		return 0, err
	}
	return hits, nil
}

// openNoLinks opens name under root read-write; a link, or an object swapped between check and open, fails it.
func openNoLinks(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	if !filepath.IsLocal(name) {
		return nil, nil, errors.New("redact: path outside root")
	}
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	dir := root
	for _, part := range parts[:len(parts)-1] {
		next, err := enterDir(dir, part)
		if dir != root {
			_ = dir.Close()
		}
		if err != nil {
			return nil, nil, err
		}
		dir = next
	}
	if dir != root {
		defer dir.Close()
	}
	leaf := parts[len(parts)-1]
	info, err := dir.Lstat(leaf)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, errors.New("redact: regular file required")
	}
	f, err := dir.OpenFile(leaf, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, errors.New("redact: file open failed")
	}
	if current, err := f.Stat(); err != nil || !os.SameFile(info, current) {
		_ = f.Close()
		return nil, nil, errors.New("redact: file changed before open")
	}
	return f, info, nil
}

// enterDir opens the real directory name under dir, refusing a link and a directory swapped in after the check.
func enterDir(dir *os.Root, name string) (*os.Root, error) {
	info, err := dir.Lstat(name)
	if err != nil || !info.IsDir() {
		return nil, errors.New("redact: path through a link or non-directory")
	}
	sub, err := dir.OpenRoot(name)
	if err != nil {
		return nil, errors.New("redact: directory open failed")
	}
	if current, err := sub.Stat("."); err != nil || !os.SameFile(info, current) {
		_ = sub.Close()
		return nil, errors.New("redact: directory changed before open")
	}
	return sub, nil
}

// A full snapshot permits JSON validation before any write, including lines and PEM blocks spanning windows.
func snapshot(f *os.File, size int64, overlap int) ([]byte, error) {
	var b []byte
	for offset := int64(0); offset < size; {
		chunk := make([]byte, min(int64(max(windowSize, overlap+1)), size-offset))
		n, err := f.ReadAt(chunk, offset)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("redact: read failed at byte %d", offset)
		}
		if n != len(chunk) {
			return nil, fmt.Errorf("redact: short read at byte %d", offset)
		}
		shared := min(overlap, len(b))
		if !bytes.Equal(b[len(b)-shared:], chunk[:shared]) {
			return nil, errors.New("redact: file changed between windows")
		}
		b = append(b, chunk[shared:]...)
		if offset+int64(n) == size {
			break
		}
		offset += int64(n - overlap)
	}
	return b, nil
}

func validLines(before, after []byte) bool {
	for len(after) > 0 {
		end := bytes.IndexByte(after, '\n')
		if end < 0 {
			end = len(after)
		} else {
			end++
		}
		if !bytes.Equal(before[:end], after[:end]) && !json.Valid(after[:end]) {
			return false
		}
		before, after = before[end:], after[end:]
	}
	return true
}

func writeChanges(f *os.File, before, after []byte) error {
	for i := 0; i < len(after); {
		if before[i] == after[i] {
			i++
			continue
		}
		end := i + 1
		for end < len(after) && before[end] != after[end] {
			end++
		}
		n, err := f.WriteAt(after[i:end], int64(i))
		if err != nil || n != end-i {
			return fmt.Errorf("redact: write failed at byte %d", i)
		}
		i = end
	}
	return nil
}

// maskWidth is the length of an existing mask at b[0]; not ok while more bytes could extend or complete it.
func maskWidth(b []byte, final bool) (int, bool) {
	if len(b) == 0 || b[0] != '[' {
		return 0, true
	}
	if !bytes.HasPrefix(b, []byte(marker)) {
		return 0, final || len(b) >= len(marker) || !strings.HasPrefix(marker, string(b))
	}
	n := len(marker)
	for n < len(b) && b[n] == '*' {
		n++
	}
	return n, final || n < len(b)
}

// Existing masks must not let a bearer shape consume following punctuation or fields.
func shapeText(b []byte) string {
	start := bytes.Index(b, []byte(marker))
	if start < 0 {
		return string(b)
	}
	shadow := bytes.Clone(b)
	for start >= 0 {
		n, _ := maskWidth(shadow[start:], true)
		for j := start; j < start+n; j++ {
			shadow[j] = ' '
		}
		next := bytes.Index(shadow[start+n:], []byte(marker))
		if next < 0 {
			break
		}
		start += n + next
	}
	return string(shadow)
}
