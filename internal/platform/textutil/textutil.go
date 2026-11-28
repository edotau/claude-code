// Package textutil holds small string, JSONC, front-matter, credential and HTTP helpers shared across the harness.
package textutil

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Title upper-cases the first rune of s ("plus" → "Plus").
func Title(s string) string {
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// Truncate cuts s to at most n runes, ending in "…" when it does not fit, so the result never exceeds n.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	j, more := runeCut(s, n-1)
	if !more {
		return s
	}
	if _, size := utf8.DecodeRuneInString(s[j:]); j+size >= len(s) {
		return s
	}
	return validPrefix(s[:j]) + "…"
}

// runeCut is the byte offset after the first n runes of s and whether s has more than n runes.
func runeCut(s string, n int) (int, bool) {
	if n < 0 {
		return 0, true
	}
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i, i < len(s)
}

// validPrefix is p, with each invalid byte replaced by U+FFFD as a []rune conversion does.
func validPrefix(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	return string([]rune(p))
}

// CutBytes is the longest prefix of s of at most n bytes that ends on a rune boundary.
func CutBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:max(n, 0)]
}

// TruncateRunes hard-cuts s to the first n runes with no ellipsis, mirroring Python's s[:n] slicing.
func TruncateRunes(s string, n int) string { return CapRunes(s, n, "") }

// CapRunes keeps the first n runes of s and appends suffix when s is longer; s is returned unchanged otherwise.
func CapRunes(s string, n int, suffix string) string {
	if len(s) <= n {
		return s
	}
	i, more := runeCut(s, n)
	if !more {
		return s
	}
	return validPrefix(s[:i]) + suffix
}

// RuneLen is the rune count of s.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

// OneLine renders s on one trimmed line: newlines/tabs become a space, runs of spaces collapse, other controls drop.
func OneLine(s string) string {
	s = strings.TrimSpace(s)
	if !strings.ContainsFunc(s, isControl) && !strings.Contains(s, "  ") {
		return s
	}
	out := make([]rune, 0, len(s))
	prevSpace := false
	for _, c := range s {
		if c == '\n' || c == '\r' || c == '\t' {
			c = ' '
		}
		if c == ' ' && prevSpace || isControl(c) {
			continue
		}
		prevSpace = c == ' '
		out = append(out, c)
	}
	return string(out)
}

func isControl(c rune) bool { return unicode.IsControl(c) }

// PluralWord picks one or many for n.
func PluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// IsDigits mirrors Python str.isdigit(): non-empty and all ASCII digits.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// HumanBytes renders a byte count compactly in binary units (1 decimal past B).
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		f /= unit
		if f < unit-0.05 {
			return fmt.Sprintf("%.1f %s", f, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", f/unit)
}

// Tokenize lowercases s and splits on any non-alphanumeric run.
func Tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// SplitCSV splits a comma-separated list, trimming blanks and dropping empty entries; nil when none remain.
func SplitCSV(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// SHA256Hex is the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ErrWriter forwards writes to W and keeps the first error in Err, so a caller checks once after a run of prints.
type ErrWriter struct {
	W   io.Writer
	Err error
}

func (e *ErrWriter) Write(p []byte) (int, error) {
	if e.Err != nil {
		return 0, e.Err
	}
	n, err := e.W.Write(p)
	e.Err = err
	return n, err
}

// SetOf is items as a set.
func SetOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// SplitNUL splits git -z style output on NUL, dropping empty fields.
func SplitNUL(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == 0 })
}

// LastLine returns the last non-empty line of s, trimmed.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, " \t\r\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// CompactCount spells n as 950, 94.4k, 200k or 1.2M: one decimal, a trailing ".0" dropped, millions from 999_950.
func CompactCount(n int64) string {
	switch {
	case n >= 999_950: // from 999_950 %.1fk would print 1000.0k
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64), ".0") + "M"
	case n >= 1000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64), ".0") + "k"
	}
	return strconv.FormatInt(n, 10)
}

// PatchKind is how an apply_patch hunk changes its file.
type PatchKind string

// The hunk header kinds of an apply_patch body.
const (
	PatchAdd    PatchKind = "add"
	PatchUpdate PatchKind = "update"
	PatchDelete PatchKind = "delete"
	PatchMove   PatchKind = "move"
)

// FileEdit is one file a tool call changes; Body holds an apply_patch hunk's "+" lines.
type FileEdit struct {
	Path string
	Kind PatchKind
	Body string
}

var patchHeaders = []struct {
	prefix string
	kind   PatchKind
}{{"*** Add File:", PatchAdd}, {"*** Update File:", PatchUpdate}, {"*** Delete File:", PatchDelete}, {"*** Move to:", PatchMove}}

// PatchEdits lists the files an apply_patch body adds, updates, deletes or moves to, in order; empty paths are skipped.
func PatchEdits(patch string) []FileEdit {
	var out []FileEdit
	var body []string
	closeHunk := func() {
		if len(out) > 0 && body != nil {
			out[len(out)-1].Body = strings.Join(body, "\n")
		}
		body = nil
	}
	for line := range strings.SplitSeq(patch, "\n") {
		if !strings.HasPrefix(line, "*** ") {
			if text, ok := strings.CutPrefix(line, "+"); ok && body != nil {
				body = append(body, text)
			}
			continue
		}
		closeHunk()
		for _, h := range patchHeaders {
			if path, ok := strings.CutPrefix(line, h.prefix); ok && strings.TrimSpace(path) != "" {
				out = append(out, FileEdit{Path: strings.TrimSpace(path), Kind: h.kind})
				body = []string{}
			}
		}
	}
	closeHunk()
	return out
}

// WriteStrings appends each part to b in order, with no intermediate string built.
func WriteStrings(b *strings.Builder, parts ...string) {
	for _, p := range parts {
		b.WriteString(p)
	}
}

// MarshalNoEscape marshals compact JSON without HTML escaping or a trailing newline.
func MarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// MarshalIndentNoEscape marshals two-space-indented JSON without HTML escaping or a trailing newline.
func MarshalIndentNoEscape(v any) ([]byte, error) {
	compact, err := MarshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// JWTClaims decodes the payload of a three-part JWT-shaped token without verifying its signature.
func JWTClaims(token string, into any) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	return err == nil && json.Unmarshal(payload, into) == nil
}

// QuoteSH single-quotes s for POSIX sh unless it contains only shell-safe characters.
func QuoteSH(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./:@%+=,-", r)
	}) == -1 {
		return s
	}
	return ShellQuote(s)
}

// QuotePath quotes a remote POSIX sh path, expanding a leading ~ through the quoted HOME variable.
func QuotePath(p string) string {
	switch {
	case p == "~":
		return `"$HOME"`
	case strings.HasPrefix(p, "~/"):
		return `"$HOME"/` + ShellQuote(strings.TrimPrefix(p, "~/"))
	default:
		return ShellQuote(p)
	}
}

// ShellQuote always single-quotes s for POSIX sh, closing and reopening around embedded quotes.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellJoin is the words as one shell command line.
func ShellJoin(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = QuoteSH(w)
	}
	return strings.Join(out, " ")
}

// ValidHost refuses an ssh destination that ssh would parse as an option or that carries whitespace/control bytes.
func ValidHost(host string) error {
	bad := func(r rune) bool { return r <= ' ' || r == 0x7f }
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsFunc(host, bad) {
		return fmt.Errorf("invalid ssh host %q", host)
	}
	return nil
}

// Nearest is the one name within edit distance 2 of want (1 for want of ≤3 runes); ok is false on none or a tie.
func Nearest(want string, names []string) (string, bool) {
	rw := []rune(want)
	limit, best, bestD, tie := 2, "", 1<<30, false
	if len(rw) <= 3 {
		limit = 1
	}
	prev, cur := make([]int, 0, 32), make([]int, 0, 32) // fits most names, so rows rarely regrow
	for _, n := range names {
		// the rune-length gap is a lower bound on the distance, so a wider gap can never be the match
		if gap := utf8.RuneCountInString(n) - len(rw); gap > limit || -gap > limit {
			continue
		}
		var d int
		d, prev, cur = distance(rw, n, prev, cur)
		switch {
		case d < bestD:
			best, bestD, tie = n, d, false
		case d == bestD:
			tie = true
		}
	}
	return best, bestD <= limit && !tie && best != ""
}

// distance is the Levenshtein distance of ra and b, reusing prev and cur as its two row buffers.
func distance(ra []rune, b string, prev, cur []int) (int, []int, []int) {
	rb := []rune(b)
	prev, cur = prev[:0], cur[:0]
	for j := 0; j <= len(rb); j++ {
		prev, cur = append(prev, j), append(cur, 0)
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)], prev, cur
}

// FirstLine returns the text before the first newline without trimming whitespace.
func FirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// FirstNonBlank returns the first line of s that is not blank, trimmed; "" when every line is blank.
func FirstNonBlank(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

var lineReaderPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 1<<20) }}

// EachLineErr joins oversized lines and includes the final partial line
func EachLineErr(r io.Reader, fn func(line []byte)) error {
	_, err := ScanRawLines(r, false, func(line []byte) {
		line = bytes.TrimSuffix(line, []byte{'\n'})
		fn(bytes.TrimSuffix(line, []byte{'\r'}))
	})
	return err
}

// ScanRawLines is EachLineErr keeping each terminator; completeOnly drops an unterminated tail; returns bytes read.
func ScanRawLines(r io.Reader, completeOnly bool, fn func(line []byte)) (int64, error) {
	br := lineReaderPool.Get().(*bufio.Reader)
	br.Reset(r)
	defer func() {
		br.Reset(nil)
		lineReaderPool.Put(br)
	}()
	var long []byte
	var read int64
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long = append(long, line...)
			continue
		}
		if long != nil {
			line, long = append(long, line...), nil
		}
		if len(line) > 0 && (!completeOnly || err == nil) {
			fn(line)
			read += int64(len(line))
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return read, err
		}
	}
}

// ReadLineCapped returns the next line with its newline; a line over max bytes is drained and flagged tooLong.
func ReadLineCapped(r *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > max {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch err {
		case bufio.ErrBufferFull:
			continue
		case nil:
			return line, tooLong, nil
		case io.EOF:
			if len(line) > 0 || tooLong {
				return line, tooLong, io.EOF
			}
		}
		return nil, false, err
	}
}

// CompareVersions orders dotted numeric versions, ignoring v and hyphen suffixes; false means parsing failed.
func CompareVersions(a, b string) (int, bool) {
	as, bs := versionParts(a), versionParts(b)
	if as == nil || bs == nil {
		return 0, false
	}
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x < y {
			return -1, true
		}
		if x > y {
			return 1, true
		}
	}
	return 0, true
}

func versionParts(v string) []int {
	core, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), "-") // drop prerelease before splitting
	var out []int
	for s := range strings.SplitSeq(core, ".") {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// BuildVersion is the module version stamped into the binary, or "devel" when none is.
var BuildVersion = sync.OnceValue(func() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "devel"
})

var majorRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(\d+)\.\d+`) })

// VersionMajor reads the major from "1.19.3", "v2.0.32" or "opencode 2.0.32"; 0 when none.
func VersionMajor(s string) int {
	m := majorRe().FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// versionRe picks the first dotted version in a --version line, never digits glued to a name ("gpt-5.1" stays a name).
var versionRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?:^|[^0-9A-Za-z.\-])v?(\d+(?:\.\d+)+)`) })

// FirstVersion returns the first dotted version in --version output ("codex-cli 0.155.0" -> "0.155.0"), else "".
func FirstVersion(line string) string {
	if m := versionRe().FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// StripJSONC removes comments and trailing commas outside strings; an unterminated /* returns data unchanged.
func StripJSONC(data []byte) []byte {
	out := make([]byte, 0, len(data))
	var st strState
	for i := 0; i < len(data); i++ {
		c := data[i]
		if st.step(c) {
			out = append(out, c)
			continue
		}
		switch {
		case c == '"':
			st.in = true
			out = append(out, c)
		case hasPair(data, i, '/', '/'):
			nl := bytes.IndexByte(data[i+2:], '\n')
			if nl < 0 {
				return out
			}
			i += 2 + nl
			out = append(out, '\n')
		case hasPair(data, i, '/', '*'):
			end := bytes.Index(data[i+2:], []byte("*/"))
			if end < 0 {
				return data
			}
			i += 2 + end + 1
		case c == '}' || c == ']':
			out = append(dropTrailingComma(out), c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// strState tracks whether the scan is inside a JSON string literal and just past a backslash.
type strState struct{ in, escaped bool }

// step consumes c while inside a string and reports whether c belonged to the string (it must be copied through).
func (st *strState) step(c byte) bool {
	if !st.in {
		return false
	}
	switch {
	case st.escaped:
		st.escaped = false
	case c == '\\':
		st.escaped = true
	case c == '"':
		st.in = false
	}
	return true
}

func hasPair(data []byte, i int, a, b byte) bool {
	return data[i] == a && i+1 < len(data) && data[i+1] == b
}

func dropTrailingComma(out []byte) []byte {
	if n := len(bytes.TrimRight(out, " \t\n\r")); n > 0 && out[n-1] == ',' {
		return append(out[:n-1], out[n:]...)
	}
	return out
}
