package memory

import (
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
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
	fence, open := "", -1
	for _, ln := range lines {
		if fence == "" && d.Grammar.starts(ln) {
			d.Entries = append(d.Entries, newEntry(ln, len(d.Entries)))
			open = len(d.Entries) - 1
		} else if open >= 0 {
			d.Entries[open].Lines = append(d.Entries[open].Lines, ln)
		} else {
			d.Hidden = append(d.Hidden, ln)
		}
		fence = nextFence(fence, ln)
	}
	for i := range d.Entries {
		d.Entries[i].ID = entryID(d.Entries[i].Lines)
	}
	return d
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

// frontEnd is the line count of a leading --- block (fences included), 0 when line 0 is not one or never closes.
func frontEnd(lines []string) int {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return i + 1
		}
	}
	return 0
}

// entryID is the last marker on an unfenced line of the entry.
func entryID(lines []string) string {
	id, fence := "", ""
	for _, ln := range lines {
		if fence == "" {
			if r, _, _, ok := marker(ln); ok && !r.Derived {
				id = r.Hex
			}
		}
		fence = nextFence(fence, ln)
	}
	return id
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

// ParseRef reads [z]<8 hex>[-<slug>]; the slug is lowercase letters, digits and hyphens.
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
	if !ok || slug == "" || strings.Trim(slug, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
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

// marker finds the last "<!-- id:REF -->" outside a backtick code span; lo and hi bound it in line.
func marker(line string) (r Ref, lo, hi int, ok bool) {
	end := len(line)
	for end > 0 {
		lo = strings.LastIndex(line[:end], markerOpen)
		if lo < 0 {
			return Ref{}, 0, 0, false
		}
		end = lo
		if strings.Count(line[:lo], "`")%2 == 1 {
			continue
		}
		body, _, found := strings.Cut(line[lo+len(markerOpen):], markerClose)
		if !found {
			continue
		}
		if r, ok = ParseRef(body); ok {
			return r, lo, lo + len(markerOpen) + len(body) + len(markerClose), true
		}
	}
	return Ref{}, 0, 0, false
}

// FrontFields reads name, description and metadata.type from the front block; false when there is none.
func (d Doc) FrontFields() (Front, bool) {
	if d.Front == nil {
		return Front{}, false
	}
	m, _ := textutil.ParseFrontmatter(strings.Join(d.Front, "\n"))
	f := Front{Name: str(m["name"]), Description: str(m["description"])}
	if md, ok := m["metadata"].(map[string]any); ok {
		f.Type = str(md["type"])
	}
	return f, true
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// WithFront returns a copy whose front lines carry f; only changed name, description and type lines are rewritten.
func (d Doc) WithFront(f Front) Doc {
	out := d
	cur, _ := d.FrontFields()
	lines := slices.Clone(d.Front)
	if lines == nil {
		lines = []string{"---", "---"}
	}
	for _, kv := range [][3]string{{"name", cur.Name, f.Name}, {"description", cur.Description, f.Description}} {
		if kv[1] != kv[2] {
			lines = setFrontLine(lines, "", kv[0], kv[2])
		}
	}
	if cur.Type != f.Type {
		lines = setFrontLine(lines, "  ", "type", f.Type)
	}
	out.Front = lines
	return out
}

// setFrontLine replaces the "<indent>key:" line, or inserts one (under metadata: for an indented key).
func setFrontLine(lines []string, indent, key, val string) []string {
	val = strings.ReplaceAll(val, "\n", " ")
	want := indent + key + ": " + val
	for i, ln := range lines {
		if strings.HasPrefix(ln, indent+key+":") && (indent != "" || !strings.HasPrefix(ln, " ")) {
			lines[i] = want
			return lines
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
