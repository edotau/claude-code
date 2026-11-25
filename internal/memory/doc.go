package memory

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Grammar says which lines start an entry; chosen from the file name, never guessed from content.
type Grammar uint8

const (
	GrammarDated    Grammar = iota // "## " starts an entry: decisionLog, sessionHistory, their archives
	GrammarBullets                 // "- " or "## " at column 0: conventions and its archives
	GrammarSections                // "## " heading, "- " bullets under it: every other .md (state files, notes)
)

// Kind is how an entry starts.
type Kind uint8

const (
	KindHeading Kind = iota // "## …"; in GrammarDated the heading and its body are one entry
	KindBullet              // a column-0 "- …" with every line up to the next start
)

// DocEntry is one addressable unit: its raw lines, id marker and trailing blank lines included.
type DocEntry struct {
	ID    string // 8 hex of its <!-- id:… --> marker; "" until settle mints one
	Kind  Kind
	Date  string // derived: YYYY-MM-DD of a "## YYYY-MM-DD — …" heading, else ""
	Seq   int    // derived: 0-based position among the file's entries at parse
	Lines []string
}

// Doc is one Markdown memory file; ParseDoc(name, b).Render() equals b for every input.
type Doc struct {
	Name    string // logicalName of the file: picks the grammar and the age order
	Grammar Grammar
	Front   []string // the leading --- block, both fences included; nil when absent
	Header  string   // the "# Title" line; "" when the first line is not one
	Hidden  []string // lines between Header and the first entry: "> contract", blanks, <!-- --> lines
	Entries []DocEntry
}

// Ref is the one id grammar: [z]<8 hex>[-<slug>].
type Ref struct {
	Hex     string // 8 lowercase hex digits
	Derived bool   // "z" prefix: a content hash (contentRef), never a stored marker
	Alias   string // slug a model or a hand wrote after "-"; Go never mints one
}

// Front is the typed view of a note's frontmatter; other keys stay untouched in Doc.Front.
type Front struct{ Name, Description, Type string }

// GrammarOf picks the grammar from the file name: idFiles and archives first, else GrammarSections.
func GrammarOf(name string) Grammar {
	if src, ok := archiveSource(filepath.Base(name)); ok { // an archive parses with its source file's grammar
		name = src + ".md"
	}
	switch logicalName(name) {
	case "decisionLog.md", "sessionHistory.md":
		return GrammarDated
	case "conventions.md":
		return GrammarBullets
	}
	return GrammarSections
}

func (g Grammar) starts(line string) bool {
	if strings.HasPrefix(line, "## ") {
		return true
	}
	return g != GrammarDated && strings.HasPrefix(line, "- ")
}

// ParseDoc splits data into front, header, hidden lines and entries; nothing is dropped or rewritten.
func ParseDoc(name string, data []byte) Doc {
	d := Doc{Name: logicalName(name), Grammar: GrammarOf(name)}
	lines := strings.Split(string(data), "\n")
	if n := frontEnd(lines); n > 0 {
		d.Front, lines = lines[:n], lines[n:]
	}
	if len(lines) > 0 && strings.HasPrefix(lines[0], "# ") {
		d.Header, lines = lines[0], lines[1:]
	}
	inFence, open := closedFenceMask(lines), -1
	for i, ln := range lines {
		if !inFence[i] && d.Grammar.starts(ln) {
			d.Entries = append(d.Entries, newEntry(ln, len(d.Entries)))
			open = len(d.Entries) - 1
		} else if open >= 0 {
			d.Entries[open].Lines = append(d.Entries[open].Lines, ln)
		} else {
			d.Hidden = append(d.Hidden, ln)
		}
	}
	for i := range d.Entries {
		d.Entries[i].ID = entryID(d.Entries[i].Lines)
	}
	return d
}

// closedFenceMask marks the lines after a fence opener through its closer; a fence that never closes is ordinary text.
func closedFenceMask(lines []string) []bool {
	in := make([]bool, len(lines))
	dead := "" // an opener of this run found no closer, so no later one of its char and length can
	for i := 0; i < len(lines); i++ {
		if nextFence("", lines[i]) == "" {
			continue
		}
		run := fenceRun(strings.TrimSpace(lines[i]))
		if dead != "" && run[0] == dead[0] && len(run) >= len(dead) {
			continue
		}
		j := i + 1
		for j < len(lines) && nextFence(run, lines[j]) != "" {
			j++
		}
		if j == len(lines) {
			dead = run
			continue
		}
		for k := i + 1; k <= j; k++ {
			in[k] = true
		}
		i = j
	}
	return in
}

func newEntry(first string, seq int) DocEntry {
	e := DocEntry{Kind: KindBullet, Seq: seq, Lines: []string{first}}
	if strings.HasPrefix(first, "## ") {
		e.Kind = KindHeading
		if h := first[3:]; len(h) >= 10 && isDate(h[:10]) {
			e.Date = h[:10]
		}
	}
	return e
}

func isDate(s string) bool {
	for i := range len(s) {
		switch i {
		case 4, 7:
			if s[i] != '-' {
				return false
			}
		default:
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		}
	}
	return true
}

// frontEnd is the line count of a leading --- block (fences included); 0 unless every line fits YAML and it closes.
func frontEnd(lines []string) int {
	if len(lines) == 0 || lines[0] != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		switch {
		case lines[i] == "---":
			return i + 1
		case !frontLine(lines[i]):
			return 0
		}
	}
	return 0
}

// frontLine reports whether ln can sit inside frontmatter: blank, comment, indented, or a "key:" line.
func frontLine(ln string) bool {
	if strings.TrimSpace(ln) == "" || ln[0] == '#' || ln[0] == ' ' || ln[0] == '\t' {
		return true
	}
	key, _, ok := splitKey(ln)
	return ok && key != "" && key[0] != '-'
}

// entryID is the id marker on the entry's first line; a marker on a later line is body text.
func entryID(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	if r, _, _, ok := marker(lines[0]); ok && !r.Derived {
		return r.Hex
	}
	return ""
}

// Render is the exact inverse of ParseDoc.
func (d Doc) Render() []byte {
	parts := slices.Clone(d.Front)
	if d.Header != "" {
		parts = append(parts, d.Header)
	}
	parts = append(parts, d.Hidden...)
	for _, e := range d.Entries {
		parts = append(parts, e.Lines...)
	}
	return []byte(strings.Join(parts, "\n"))
}

// ParseRef reads [z]<8 hex>[-<slug>]; the slug is lowercase letters and digits, hyphens inside.
func ParseRef(s string) (Ref, bool) {
	var r Ref
	if rest, ok := strings.CutPrefix(s, contentRefPrefix); ok {
		r.Derived, s = true, rest
	}
	if len(s) < 8 || !isHex(s[:8]) {
		return Ref{}, false
	}
	r.Hex, s = s[:8], s[8:]
	if s == "" {
		return r, true
	}
	slug, ok := strings.CutPrefix(s, "-")
	if !ok || slug == "" || strings.Trim(slug, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" ||
		slug[0] == '-' || slug[len(slug)-1] == '-' {
		return Ref{}, false
	}
	r.Alias = slug
	return r, true
}

func isHex(s string) bool {
	return s != "" && strings.Trim(s, "0123456789abcdef") == ""
}

// String formats the ref back to its one textual form.
func (r Ref) String() string {
	s := r.Hex
	if r.Derived {
		s = contentRefPrefix + s
	}
	if r.Alias != "" {
		s += "-" + r.Alias
	}
	return s
}

const markerOpen, markerClose = "<!-- id:", " -->"

// marker finds the last "<!-- id:REF -->" outside a backtick code span in one forward scan; lo and hi bound it.
func marker(line string) (r Ref, lo, hi int, ok bool) {
	ticks, pos, closeAt := 0, 0, -1
	for {
		i := strings.Index(line[pos:], markerOpen)
		if i < 0 {
			return r, lo, hi, ok
		}
		start := pos + i
		ticks += strings.Count(line[pos:start], "`")
		pos = start + len(markerOpen)
		if ticks%2 == 1 {
			continue
		}
		if closeAt < pos {
			c := strings.Index(line[pos:], markerClose)
			if c < 0 {
				return r, lo, hi, ok
			}
			closeAt = pos + c
		}
		if ref, good := ParseRef(line[pos:closeAt]); good {
			r, lo, hi, ok = ref, start, closeAt+len(markerClose), true
		}
	}
}

// ErrFrontFlow is returned when a type change would have to edit an inline (flow-style) metadata map.
var ErrFrontFlow = errors.New("memory: frontmatter metadata is not a block map; edit it by hand")

// FrontFields reads name, description and metadata.type from the front block; false when there is none.
func (d Doc) FrontFields() (Front, bool, error) {
	if d.Front == nil {
		return Front{}, false, nil
	}
	f, err := readFront(d.Front)
	return f, true, err
}

// readFront reads the three typed scalars from front lines (fences included); a malformed quoted value is an error.
func readFront(front []string) (Front, error) {
	var f Front
	if len(front) < 2 {
		return f, nil
	}
	body := front[1 : len(front)-1]
	for i := 0; i < len(body); i++ {
		key, val, ok := splitKey(body[i])
		if !ok {
			continue
		}
		j := continuationEnd(body, i+1)
		if err := f.set(key, val, body[i+1:j]); err != nil {
			return Front{}, fmt.Errorf("frontmatter %s: %w", key, err)
		}
		i = j - 1
	}
	return f, nil
}

// continuationEnd is the index after the indented lines that start at from.
func continuationEnd(lines []string, from int) int {
	for from < len(lines) && strings.TrimSpace(lines[from]) != "" && (lines[from][0] == ' ' || lines[from][0] == '\t') {
		from++
	}
	return from
}

func (f *Front) set(key, val string, cont []string) (err error) {
	switch key {
	case "name":
		f.Name, err = foldedScalar(val, cont)
	case "description":
		f.Description, err = foldedScalar(val, cont)
	case "metadata":
		f.Type, err = metaType(val, cont)
	}
	return err
}

// splitKey splits a top-level "key: value" line; ok is false for indented, comment or non-key lines.
func splitKey(ln string) (key, val string, ok bool) {
	if ln == "" || ln[0] == ' ' || ln[0] == '\t' || ln[0] == '#' {
		return "", "", false
	}
	k, v, found := strings.Cut(ln, ":")
	if !found || (v != "" && v[0] != ' ' && v[0] != '\t') {
		return "", "", false
	}
	return strings.TrimSpace(k), v, true
}

// foldedScalar reads a value and, for a plain or block scalar, folds its indented continuation lines in.
func foldedScalar(val string, cont []string) (string, error) {
	s, plain, err := scalar(val)
	if err != nil || !plain {
		return s, err
	}
	for _, c := range cont {
		if t := strings.TrimSpace(c); t != "" {
			s = strings.TrimSpace(s + " " + t)
		}
	}
	return s, nil
}

// metaType reads metadata.type from a block map (the indented lines) or a one-line flow map.
func metaType(val string, cont []string) (string, error) {
	if t := strings.TrimSpace(val); strings.HasPrefix(t, "{") {
		cont = strings.Split(strings.TrimSuffix(strings.TrimPrefix(t, "{"), "}"), ",")
	}
	for _, c := range cont {
		if k, v, ok := strings.Cut(strings.TrimSpace(c), ":"); ok && strings.TrimSpace(k) == "type" {
			s, _, err := scalar(v)
			return s, err
		}
	}
	return "", nil
}

// scalar reads one value: quoted, else plain up to " #"; plain is false for a quoted one.
func scalar(val string) (s string, plain bool, err error) {
	val = strings.TrimSpace(val)
	switch {
	case val == "" || val[0] == '#' || val[0] == '|' || val[0] == '>':
		return "", true, nil
	case val[0] == '"' || val[0] == '\'':
		s, err = unquote(val)
		return s, false, err
	}
	val, _, _ = strings.Cut(val, " #")
	return strings.TrimSpace(val), true, nil
}

// unquote reads a quoted scalar; text after the closing quote must be a comment.
func unquote(val string) (string, error) {
	q := val[0]
	var b strings.Builder
	for i := 1; i < len(val); i++ {
		c := val[i]
		switch {
		case q == '"' && c == '\\' && i+1 < len(val):
			i++
			b.WriteString(escaped(val[i]))
		case c == q && q == '\'' && i+1 < len(val) && val[i+1] == q:
			b.WriteByte(q) // a doubled single quote is one quote
			i++
		case c == q:
			return b.String(), afterClose(val[i+1:])
		default:
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf("unterminated quote in %q", val)
}

// escaped is a double-quoted backslash pair: \\ and \" unescape, any other pair stays as written.
func escaped(c byte) string {
	if c == '\\' || c == '"' {
		return string(c)
	}
	return "\\" + string(c)
}

func afterClose(rest string) error {
	if rest = strings.TrimSpace(rest); rest != "" && rest[0] != '#' {
		return fmt.Errorf("text after the closing quote: %q", rest)
	}
	return nil
}

// encodeScalar writes v so readFront returns it unchanged: plain when safe, else double-quoted.
func encodeScalar(v string) (string, error) {
	for _, r := range v {
		if r != '\t' && unicode.IsControl(r) {
			return "", fmt.Errorf("frontmatter value %q holds a control character or newline", v)
		}
	}
	if plainSafe(v) {
		return v, nil
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`, nil
}

// plainSafe reports whether v reads back as the same string and type when written unquoted.
func plainSafe(v string) bool {
	if v == "" || v != strings.TrimSpace(v) || strings.Contains(v, "\t") {
		return false
	}
	if strings.ContainsRune("[{*&!|>'\"%@`#,", rune(v[0])) || strings.Contains(v+" ", ": ") || strings.Contains(v, " #") {
		return false
	}
	if strings.ContainsRune("-?:", rune(v[0])) && (len(v) == 1 || v[1] == ' ') {
		return false
	}
	return plainTyped(v)
}

// plainTyped is false for a string YAML would read as a bool, null or number.
func plainTyped(v string) bool {
	low := strings.ToLower(v)
	switch low {
	case "true", "false", "yes", "no", "on", "off", "y", "n", "null", "~":
		return false
	}
	_, err := strconv.ParseFloat(v, 64)
	return err != nil && !strings.HasPrefix(low, "0x") && !strings.HasPrefix(low, "0o")
}

// WithFront returns a copy whose front lines carry f; only changed name, description and type lines are rewritten.
func (d Doc) WithFront(f Front) (Doc, error) {
	cur, _, err := d.FrontFields()
	if err != nil {
		return d, err
	}
	lines := slices.Clone(d.Front)
	if lines == nil {
		lines = []string{"---", "---"}
	}
	for _, kv := range [][3]string{{"name", cur.Name, f.Name}, {"description", cur.Description, f.Description}} {
		if kv[1] == kv[2] {
			continue
		}
		enc, err := encodeScalar(kv[2])
		if err != nil {
			return d, err
		}
		lines = setFrontLine(lines, "", kv[0], enc)
	}
	if cur.Type != f.Type {
		enc, err := encodeScalar(f.Type)
		if err != nil {
			return d, err
		}
		if slices.ContainsFunc(lines, inlineMetadata) {
			return d, ErrFrontFlow
		}
		lines = setFrontLine(lines, "  ", "type", enc)
	}
	out := d
	out.Front = lines
	return out, nil
}

func inlineMetadata(ln string) bool {
	rest, ok := strings.CutPrefix(ln, "metadata:")
	rest = strings.TrimSpace(rest)
	return ok && rest != "" && rest[0] != '#'
}

// setFrontLine replaces the "<indent>key:" line (and a top-level key's continuation lines), or inserts one.
func setFrontLine(lines []string, indent, key, val string) []string {
	want := indent + key + ": " + val
	for i, ln := range lines {
		if strings.HasPrefix(ln, indent+key+":") && (indent != "" || !strings.HasPrefix(ln, " ")) {
			end := i + 1
			if indent == "" {
				end = continuationEnd(lines[:len(lines)-1], end)
			}
			return slices.Replace(lines, i, end, want)
		}
	}
	at := len(lines) - 1
	if indent != "" {
		for i, ln := range lines {
			if strings.HasPrefix(ln, "metadata:") {
				return slices.Insert(lines, i+1, want)
			}
		}
		return slices.Insert(lines, at, "metadata:", want)
	}
	return slices.Insert(lines, at, want)
}

// Section is the entry range [lo, hi) under the "## heading" entry; -1, -1 when absent.
func (d Doc) Section(heading string) (lo, hi int) {
	for i, e := range d.Entries {
		if e.Kind == KindHeading && strings.TrimSpace(e.Lines[0]) == "## "+heading {
			hi = len(d.Entries)
			for j := i + 1; j < len(d.Entries); j++ {
				if d.Entries[j].Kind == KindHeading {
					hi = j
					break
				}
			}
			return i + 1, hi
		}
	}
	return -1, -1
}

// OldestFirst returns the entry indexes in [lo, hi) by age, oldest first; Date and Seq are the keys.
func (d Doc) OldestFirst(lo, hi int) []int {
	var idx []int
	for i := max(lo, 0); i < min(hi, len(d.Entries)); i++ {
		idx = append(idx, i)
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		ea, eb := d.Entries[a], d.Entries[b]
		if d.Grammar == GrammarDated && ea.Date != eb.Date {
			return strings.Compare(ea.Date, eb.Date)
		}
		if d.Grammar == GrammarBullets { // bottom-appended: lower Seq is older
			return ea.Seq - eb.Seq
		}
		return eb.Seq - ea.Seq // newest-first files: higher Seq is older
	})
	return idx
}
