package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
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
	Row   int64  // the store row the entry was loaded from, kept through rewrites; 0 for none
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

// GrammarOf picks the grammar from the file name: idFiles and archives first, else GrammarSections.
func GrammarOf(name string) Grammar {
	if src, ok := ArchiveSource(filepath.Base(name)); ok { // an archive parses with its source file's grammar
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
	spans, pos, closeAt := codeSpans(line), 0, -1
	for {
		i := strings.Index(line[pos:], markerOpen)
		if i < 0 {
			return r, lo, hi, ok
		}
		start := pos + i
		pos = start + len(markerOpen)
		for len(spans) > 0 && spans[0][1] <= start {
			spans = spans[1:]
		}
		if len(spans) > 0 && spans[0][0] <= start {
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

var backtickRuns = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("`+") })

// codeSpans pairs equal-length backtick runs; differing runs inside a span are literal code.
func codeSpans(line string) [][]int {
	runs := backtickRuns().FindAllStringIndex(line, -1)
	next, last := make([]int, len(runs)), map[int]int{}
	for i := len(runs) - 1; i >= 0; i-- {
		n := runs[i][1] - runs[i][0]
		next[i] = -1
		if j, ok := last[n]; ok {
			next[i] = j
		}
		last[n] = i
	}
	var spans [][]int
	for i := 0; i < len(runs); i++ {
		slashes := 0
		for j := runs[i][0] - 1; j >= 0 && line[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 != 0 {
			continue
		}
		if j := next[i]; j >= 0 {
			spans = append(spans, []int{runs[i][0], runs[j][1]})
			i = j
		}
	}
	return spans
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

// readFile is os.ReadFile for typed bank files; a var so tests can count how often one file is read.
var readFile = os.ReadFile

// indexLineMax keeps routing lines scannable; detail lives in the entry itself.
const indexLineMax = 120

// entryBlocks splits by the grammar the file NAME implies: dated logs on "## " only, others also on "- ".
func entryBlocks(base, body string) (header string, blocks []string) {
	return splitOn(body, entryStart(base))
}

// entryStart is the line test that opens an entry block in the file named base.
func entryStart(base string) func(string) bool {
	if base == "decisionLog.md" || base == "sessionHistory.md" {
		return func(ln string) bool { return strings.HasPrefix(ln, "## ") }
	}
	return func(ln string) bool { return strings.HasPrefix(ln, "- ") || strings.HasPrefix(ln, "## ") }
}

// splitHeadingBlocks splits a newest-first log into its header and "## " entries (bodies may hold bullets).
func splitHeadingBlocks(body string) (header string, blocks []string) {
	return splitOn(body, func(ln string) bool { return strings.HasPrefix(ln, "## ") })
}

// nextFence returns the open fence's marker run after ln: an opener sets it, only a bare run of its character closes.
func nextFence(open, ln string) string {
	t := strings.TrimSpace(ln)
	if open != "" {
		if fenceCloses(t, open) {
			return ""
		}
		return open
	}
	if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
		return fenceRun(t)
	}
	return ""
}

// fenceRun is the leading run of fence characters in a trimmed fence line ("```" for "```go").
func fenceRun(trimmed string) string {
	n := 0
	for n < len(trimmed) && trimmed[n] == trimmed[0] {
		n++
	}
	return trimmed[:n]
}

// fenceCloses reports whether trimmed is a bare run of open's character at least as long as open.
func fenceCloses(trimmed, open string) bool {
	return len(trimmed) >= len(open) && strings.Trim(trimmed, open[:1]) == ""
}

// splitOn is the fence-aware splitter: a marker-shaped line inside a fence never starts a block.
func splitOn(body string, isMarker func(string) bool) (header string, blocks []string) {
	starts := blockOffsets(body, isMarker)
	header = body
	if len(starts) > 0 {
		header = body[:starts[0]]
	}
	for i, s := range starts {
		end := len(body)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		blocks = append(blocks, strings.TrimRight(body[s:end], "\n"))
	}
	return strings.TrimRight(header, "\n"), blocks
}

// blockOffsets is the byte offset in body of each block splitOn returns, so blocks[i] is a prefix of body[starts[i]:].
func blockOffsets(body string, isMarker func(string) bool) []int {
	var starts []int
	lines := strings.Split(body, "\n")
	front := frontEnd(lines)
	fence, off := "", 0 // fence: marker run of the open fence, "" outside one
	for i, ln := range lines {
		if i >= front && fence == "" && isMarker(ln) {
			starts = append(starts, off)
		}
		if i >= front {
			fence = nextFence(fence, ln)
		}
		off += len(ln) + 1
	}
	return starts
}

// rejoin reassembles a bank file from its header and kept blocks.
func rejoin(header string, blocks []string) string {
	var b strings.Builder
	if header != "" {
		b.WriteString(header)
		b.WriteString("\n")
	}
	for _, blk := range blocks {
		b.WriteString("\n")
		b.WriteString(blk)
		b.WriteString("\n")
	}
	out := strings.TrimLeft(b.String(), "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// appendBlocks appends blocks to path, creating it with a minimal header when absent.
func appendBlocks(path string, blocks []string) error {
	existing, err := readFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err // never overwrite an archive we could not read
	}
	if fenceOpenAtEnd(string(existing)) {
		return fmt.Errorf("archive %s ends inside a code fence", path)
	}
	ok, werr := writeIfStill(path, existing, err == nil, []byte(appendedText(path, string(existing), blocks)), 0o600)
	if werr != nil {
		return werr
	}
	if !ok {
		return fmt.Errorf("archive %s changed outside the bank lock; append kept in the conflict copy", path)
	}
	return nil
}

// appendedText is body with blocks appended, under a minimal header when body is blank.
func appendedText(path, body string, blocks []string) string {
	if strings.TrimSpace(body) == "" {
		body = "# " + strings.TrimSuffix(filepath.Base(path), ".md") + " (archived)\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	var b strings.Builder
	b.WriteString(body)
	tail := body
	for _, block := range blocks {
		if !strings.HasSuffix(tail, "\n\n") {
			b.WriteByte('\n')
		}
		b.WriteString(block)
		tail = block
		if !strings.HasSuffix(block, "\n") {
			b.WriteByte('\n')
			tail += "\n"
		}
	}
	return b.String()
}

// cleanNewBlock puts a blank line around each ATX heading, list and fence in new text; fence and comment bodies stay.
func cleanNewBlock(block string) string {
	lines := strings.Split(block, "\n")
	out := make([]string, 0, len(lines)+4)
	fence, inList, inCmt, closed := "", false, false, false
	for _, ln := range lines {
		wasFence := fence
		inFence := fence != "" || inCmt
		if !inCmt {
			fence = nextFence(fence, ln)
		}
		opens := !inFence && fence != ""
		if fence == "" {
			inCmt = openComment(inCmt, ln)
		}
		prevBlank := len(out) == 0 || strings.TrimSpace(out[len(out)-1]) == ""
		if (opens && !prevBlank) || (closed && !prevBlank && strings.TrimSpace(ln) != "") ||
			(!inFence && !closed && len(out) > 0 && gapBefore(out[len(out)-1], ln, inList)) {
			out = append(out, "")
		}
		closed = wasFence != "" && fence == ""
		out = append(out, ln)
		switch {
		case inFence:
		case strings.TrimSpace(ln) == "":
			inList = false
		case isListItem(ln):
			inList = true
		case !continuesItem(ln):
			inList = false
		}
	}
	return strings.Join(out, "\n")
}

// openComment reports whether an HTML comment is still open after ln.
func openComment(open bool, ln string) bool {
	for {
		if open {
			_, rest, ok := strings.Cut(ln, "-->")
			if !ok {
				return true
			}
			ln, open = rest, false
			continue
		}
		_, rest, ok := strings.Cut(ln, "<!--")
		if !ok {
			return false
		}
		ln, open = rest, true
	}
}

// gapBefore reports whether new text needs a blank line between prev and ln (MD022, MD032).
func gapBefore(prev, ln string, inList bool) bool {
	switch {
	case strings.TrimSpace(prev) == "" || strings.TrimSpace(ln) == "":
		return false
	case isATXHeading(prev) || isATXHeading(ln):
		return true
	case isListItem(ln):
		return !inList
	}
	return inList && !continuesItem(ln)
}

func isATXHeading(ln string) bool {
	rest := strings.TrimLeft(ln, "#")
	n := len(ln) - len(rest)
	return n >= 1 && n <= 6 && strings.HasPrefix(rest, " ")
}

func isListItem(ln string) bool {
	ln = strings.TrimLeft(ln, " \t")
	if len(ln) < 2 {
		return false
	}
	if strings.ContainsRune("-*+", rune(ln[0])) {
		return ln[1] == ' '
	}
	n := 0
	for n < len(ln) && ln[n] >= '0' && ln[n] <= '9' {
		n++
	}
	return n > 0 && n+1 < len(ln) && (ln[n] == '.' || ln[n] == ')') && ln[n+1] == ' '
}

// continuesItem reports whether ln stays inside the list item above: an indented line or an HTML comment.
func continuesItem(ln string) bool {
	return strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "\t") || strings.HasPrefix(ln, "<!--")
}

// entriesWithPrefix returns every line starting with prefix, prefix stripped and truncated, in file order.
func entriesWithPrefix(path, prefix string) ([]string, error) {
	data, err := readIfExists(path)
	if err != nil {
		return nil, err
	}
	var out []string
	lines := strings.Split(string(data), "\n")
	fence := "" // marker run of the open fence; a prefixed line inside one is not an entry
	for i, line := range lines {
		inFence := fence != ""
		fence = nextFence(fence, line)
		if inFence {
			continue
		}
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			e := truncateLine(stripID(rest))
			if id := ownerID(wrappedBlock(lines, i)); id != "" {
				e += " [" + id + "]"
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// wrappedBlock is lines[i] and the indented continuation lines the formatter wrapped under it, joined.
func wrappedBlock(lines []string, i int) string {
	end := i + 1
	for end < len(lines) && strings.HasPrefix(lines[end], " ") && strings.TrimSpace(lines[end]) != "" {
		end++
	}
	return strings.Join(lines[i:end], "\n")
}

// truncateLine caps s at indexLineMax bytes, cutting at a word boundary, rune-safe.
func truncateLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= indexLineMax {
		return s
	}
	cut := strings.ToValidUTF8(s[:indexLineMax], "")
	if i := strings.LastIndexByte(cut, ' '); i > indexLineMax/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// idMarker is a block's persisted ID: assigned once at settle, carried by the block through edits and archive moves.
var idMarker = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\s*<!-- id:([0-9a-f]{8}) -->`)
})

// contentRefPrefix leads a derived block ref; 'z' is never hex, so it cannot collide with a real id marker.
const contentRefPrefix = "z"

// contentRef is the stable ref for an unmarked block: the prefix plus 4 bytes of sha256 of its normalized text.
func contentRef(block string) string {
	return contentRefPrefix + textutil.SHA256Hex(appendFields(nil, block))[:8]
}

// blockRef is the block's persisted id, else fallback, else its contentRef.
func blockRef(block, fallback string) string {
	if id := ownerID(block); id != "" {
		return id
	}
	if fallback != "" {
		return fallback
	}
	return contentRef(block)
}

// idFiles are the typed files whose blocks get IDs, with the prefix that starts a block.
var idFiles = map[string]string{"decisionLog.md": "## ", "sessionHistory.md": "## ", "conventions.md": "- "}

// blockID is the persisted ID on line ("" when it has none).
func blockID(line string) string {
	if spans := idSpans(line); len(spans) > 0 {
		return lastIDMarker(line[spans[0][0]:spans[0][1]])
	}
	return ""
}

// ownerID is the id a block answers to: its last unfenced marker, which may be wrapped or stacked below the first line.
func ownerID(block string) string {
	if !strings.ContainsAny(block, "`~") {
		return lastIDMarker(block)
	}
	lines := strings.Split(block, "\n")
	line, span, total := lastMarker(lines, fencedLines(lines))
	if total == 0 {
		return ""
	}
	return blockID(lines[line][span[0]:span[1]])
}

// lastIDMarker is the id of the last `<!-- id:xxxxxxxx -->` in text, "" when there is none (no regexp scan).
func lastIDMarker(text string) string {
	for end := len(text); ; {
		i := strings.LastIndex(text[:end], "<!-- id:")
		if i < 0 {
			return ""
		}
		if rest := text[i+len("<!-- id:"):]; len(rest) >= 12 && rest[8:12] == " -->" && isHex(rest[:8]) {
			return rest[:8]
		}
		end = i
	}
}

// stripID removes the ID marker for display.
func stripID(s string) string { return stripMarkers(s, false, nil) }

// idSpans excludes inline code examples from the legacy marker grammar used by settle.
func idSpans(line string) [][]int {
	spans, code := idMarker().FindAllStringIndex(line, -1), codeSpans(line)
	out := spans[:0]
	for _, sp := range spans {
		for len(code) > 0 && code[0][1] <= sp[0] {
			code = code[1:]
		}
		if len(code) == 0 || code[0][0] >= sp[1] {
			out = append(out, sp)
		}
	}
	return out
}

// assignIDTexts stamps unmarked blocks, collapses stacked markers to the oldest and returns each id file's text.
func assignIDTexts(now time.Time) (int, map[string]string, error) {
	n := 0
	texts := map[string]string{}
	for name, prefix := range idFiles {
		stamped, text, err := assignFileIDs(name, prefix, now)
		n += stamped
		if err != nil {
			return n, texts, err
		}
		if text != "" {
			texts[name] = text
		}
	}
	return n, texts, nil
}

// assignFileIDs is assignIDs for one typed file, returning its settled text; a missing file is skipped ("" text).
func assignFileIDs(name, prefix string, now time.Time) (int, string, error) {
	path := Path(name)
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	res, n := stampIDs(name, prefix, string(data), now)
	if res != string(data) {
		ok, err := writeIfStill(path, data, true, []byte(res), 0o600)
		if err != nil || !ok {
			return n, "", err // an outside edit stays; the later steps read it fresh
		}
	}
	return n, res, nil
}

// stampIDs is assignFileIDs on text in memory: the settled text (changed blocks wrapped) and the blocks it stamped.
func stampIDs(name, prefix, text string, now time.Time) (string, int) {
	n := 0
	lines := strings.Split(text, "\n")
	starts := blockStarts(lines, prefix)
	out := make([]string, 0, len(lines))
	head := len(lines)
	if len(starts) > 0 {
		head = starts[0]
	}
	out = append(out, lines[:head]...)
	for k, i := range starts {
		end := len(lines)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		if !strings.HasPrefix(lines[i], prefix) {
			out = append(out, lines[i:end]...)
			continue
		}
		block, stamped := settleBlockID(name, i, lines[i:end], now)
		for salt := len(lines); stamped == 1 && strings.Contains(text, "<!-- id:"+blockID(block[0])+" -->"); salt++ {
			block, _ = settleBlockID(name, salt, lines[i:end], now) // an id text already carries: redraw past its end
		}
		n += stamped
		if !slices.Equal(block, lines[i:end]) { // only a block settle changed is wrapped; the rest keeps its bytes
			block = strings.Split(formatBankMarkdown(strings.Join(block, "\n")), "\n")
		}
		out = append(out, block...)
	}
	return strings.Join(out, "\n"), n
}

// blockStarts includes unstamped heading boundaries, so stamping cannot format the next entry's body.
func blockStarts(lines []string, prefix string) []int {
	var starts []int
	front := frontEnd(lines)
	fence := ""
	for i, l := range lines {
		if i < front {
			continue
		}
		if fence == "" && (strings.HasPrefix(l, "## ") || strings.HasPrefix(l, prefix)) && strings.TrimSpace(strings.TrimPrefix(l, prefix)) != "" {
			starts = append(starts, i)
		}
		fence = nextFence(fence, l)
	}
	return starts
}

// fencedLines flags each line that is a code fence or sits inside one; markers there are examples, not ids.
func fencedLines(block []string) []bool {
	out := make([]bool, len(block))
	fence := ""
	for i, l := range block {
		next := nextFence(fence, l)
		out[i] = fence != "" || next != ""
		fence = next
	}
	return out
}

// settleBlockID leaves one id marker (the oldest, i.e. the last, else a fresh one); fenced lines are never touched.
func settleBlockID(name string, idx int, block []string, now time.Time) ([]string, int) {
	fenced := fencedLines(block)
	keepLine, keepSpan, total := lastMarker(block, fenced)
	if total == 0 {
		first, ending := strings.TrimSuffix(block[0], "\r"), ""
		if strings.HasSuffix(first, "  ") {
			first, ending = strings.TrimRight(first, " "), "  "
		}
		if strings.HasSuffix(block[0], "\r") {
			ending += "\r"
		}
		return append([]string{first + freshID(name, idx, block[0], now) + ending}, block[1:]...), 1
	}
	if total == 1 {
		return block, 0
	}
	kept := " " + strings.TrimSpace(block[keepLine][keepSpan[0]:keepSpan[1]])
	out := dropMarkers(block, fenced, keepLine, keepSpan)
	if keepLine != 0 {
		out[0] = strings.TrimRight(out[0], " \t") + kept
	}
	return out, 0
}

// dropMarkers removes every id marker but the survivor; a non-first line left blank by it disappears.
func dropMarkers(block []string, fenced []bool, keepLine int, keepSpan []int) []string {
	out := make([]string, 0, len(block))
	for i, l := range block {
		if !fenced[i] && idMarker().MatchString(l) {
			res := stripMarkers(l, i == 0 && i == keepLine, keepSpan) // a first-line survivor stays where it is
			if strings.TrimSpace(res) == "" && i != 0 {
				continue
			}
			l = res
		}
		out = append(out, l)
	}
	return out
}

// lastMarker finds the last unfenced id marker in block: its line, its span and how many markers there are.
func lastMarker(block []string, fenced []bool) (keepLine int, keepSpan []int, total int) {
	keepLine = -1
	for i, l := range block {
		if fenced[i] {
			continue
		}
		for _, sp := range idSpans(l) {
			keepLine, keepSpan, total = i, sp, total+1
		}
	}
	return keepLine, keepSpan, total
}

// freshID is the new ` <!-- id:xxxx -->` suffix for a block; idx is in the hash so identical lines never collide.
func freshID(name string, idx int, line string, now time.Time) string {
	key := "\x00" + name + "\x00" + strconv.Itoa(idx) + "\x00" + line + "\x00" + now.Format(time.RFC3339Nano)
	return " <!-- id:" + textutil.SHA256Hex([]byte(key))[:8] + " -->"
}

// stripMarkers removes every id marker from l, except the one at keepSpan when keepHere.
func stripMarkers(l string, keepHere bool, keepSpan []int) string {
	var b strings.Builder
	prev := 0
	for _, sp := range idSpans(l) {
		b.WriteString(l[prev:sp[0]])
		if keepHere && sp[0] == keepSpan[0] {
			b.WriteString(l[sp[0]:sp[1]])
		}
		prev = sp[1]
	}
	b.WriteString(l[prev:])
	return b.String()
}

// errNoBlock is returned when no block in the store carries the requested ID.
var errNoBlock = errors.New("no memory block with that id")

// ShowBlock finds the block for ref (an id marker or a contentRef) anywhere in the store, archives included.
func ShowBlock(ref string) (string, string, error) {
	r, _ := ParseRef(ref)
	derived := r.Derived
	var foundPath, foundBlock string
	err := filepath.WalkDir(paths.MemoryStoreDir(), func(p string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist): // no store yet: nothing to find
			return nil
		case err != nil:
			return err
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
		case d.IsDir():
			return nil
		}
		b, ok, err := blockWithRef(p, ref, derived)
		if err == nil && ok {
			foundPath, foundBlock = p, b
			return filepath.SkipAll // stop at the first file holding it: the rest of the store need not be read
		}
		return err
	})
	if err != nil {
		return "", "", err
	}
	if foundPath == "" {
		return "", "", errNoBlock
	}
	return foundPath, foundBlock, nil
}

// blockWithRef returns the block in file p owning ref (an id marker), or the last one whose contentRef is ref.
func blockWithRef(p, ref string, derived bool) (found string, ok bool, err error) {
	if !strings.HasSuffix(p, ".md") {
		return "", false, nil
	}
	if !derived {
		found, err = blockIn(p, logicalName(p), ref)
		return found, found != "", err
	}
	data, err := readIfExists(p)
	if err != nil {
		return "", false, err
	}
	_, blocks := entryBlocks(logicalName(p), string(data))
	base := filepath.Base(p) // native topics and knowledge notes are indexed whole, under their own grammar
	blocks = append(blocks, parseBlocks(segNative, base, data)...)
	blocks = append(blocks, parseBlocks(segKnowledge, base, data)...)
	for _, b := range blocks {
		if contentRef(b) == ref {
			found, ok = b, true
		}
	}
	return found, ok, nil
}

// logicalName maps an archive file (decisionLog-2026-10.md) back to its typed file's grammar.
func logicalName(path string) string {
	base := filepath.Base(path)
	if _, ok := idFiles[base]; ok {
		return base
	}
	if src, ok := ArchiveSource(base); ok && idFiles[src+".md"] != "" {
		return src + ".md"
	}
	if _, rest, ok := strings.Cut(base, "--"); ok && rest != "" { // a flat file: <key>--<name>
		if n := logicalName(rest); n != rest || idFiles[rest] != "" {
			return n
		}
	}
	return base
}
