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
	Skipped int
	longest int
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
	m := &Matcher{}
	unique := make(map[string]bool)
	for _, val := range vals {
		for _, leaf := range leaves(val) {
			if !eligible(leaf) {
				m.Skipped++
				continue
			}
			for _, v := range variants(leaf) {
				if !unique[v] {
					m.needles[v[0]] = append(m.needles[v[0]], v)
					unique[v] = true
					m.longest = max(m.longest, len(v))
				}
			}
		}
	}
	for i := range m.needles {
		sort.Slice(m.needles[i], func(a, b int) bool { return len(m.needles[i][a]) > len(m.needles[i][b]) })
	}
	return m
}

func leaves(val string) []string {
	var obj any
	if json.Unmarshal([]byte(val), &obj) != nil {
		return []string{val}
	}
	switch obj.(type) {
	case map[string]any, []any:
	default:
		return []string{val}
	}
	var out []string
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, item := range x {
				visit(item)
			}
		case map[string]any:
			for _, item := range x {
				visit(item)
			}
		}
	}
	visit(obj)
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
	hits := m.MaskValues(b)
	for _, span := range textutil.SecretSpans(shapeText(b)) {
		fill(b[span[0]:span[1]])
		hits++
	}
	return hits
}

// MaskValues masks only exact vault values and their encodings, for text whose structure a shape span could cut.
func (m *Matcher) MaskValues(b []byte) int {
	hits := 0
	for i := 0; i < len(b); {
		if n := maskWidth(b[i:]); n > 0 {
			i += n
			continue
		}
		width := m.match(b[i:])
		if width == 0 {
			i++
			continue
		}
		end := i + width
		for j := i + 1; j < end; j++ {
			end = max(end, j+m.match(b[j:]))
		}
		fill(b[i:end])
		hits++
		i = end
	}
	return hits
}

func (m *Matcher) match(b []byte) int {
	for _, needle := range m.needles[b[0]] {
		if bytes.HasPrefix(b, []byte(needle)) {
			return len(needle)
		}
	}
	return 0
}

func (m *Matcher) String(s string) string {
	b := []byte(s)
	if m.Mask(b) == 0 {
		return s
	}
	return string(b)
}

func File(path string, m *Matcher, idle time.Duration) (hits int, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, errors.New("redact: file metadata unavailable")
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("redact: regular file required")
	}
	if time.Since(info.ModTime()) < idle {
		return 0, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, errors.New("redact: file open failed")
	}
	defer func() {
		if f.Close() != nil {
			err = errors.New("redact: file close failed")
		}
	}()
	current, err := f.Stat()
	if err != nil || !os.SameFile(info, current) {
		return 0, errors.New("redact: file changed before open")
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
	if filepath.Ext(path) == ".jsonl" && !validLines(before, after) {
		return 0, errors.New("redact: invalid changed JSON line")
	}
	current, err = f.Stat()
	if err != nil || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return 0, errors.New("redact: file changed during scan")
	}
	if err := writeChanges(f, before, after); err != nil {
		return 0, err
	}
	return hits, nil
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

func maskWidth(b []byte) int {
	if len(b) == 0 || b[0] != '[' || !bytes.HasPrefix(b, []byte(marker)) {
		return 0
	}
	n := len(marker)
	for n < len(b) && b[n] == '*' {
		n++
	}
	return n
}

// Existing masks must not let a bearer shape consume following punctuation or fields.
func shapeText(b []byte) string {
	start := bytes.Index(b, []byte(marker))
	if start < 0 {
		return string(b)
	}
	shadow := bytes.Clone(b)
	for start >= 0 {
		n := maskWidth(shadow[start:])
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
