package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/table"
)

const (
	pickNone = iota
	pickChoose
	pickQuit
)

// pickState is the panel's pure state, separated from I/O so key handling tests without a TTY.
type pickState struct {
	rows   []string     // rows laid out at the current terminal width, re-laid on every draw
	hay    []string     // the same rows unclipped and lower-cased, so the filter matches cut-off text; nil = rows
	heads  map[int]bool // rows that are section titles: drawn after a blank line, never selectable
	sel    int
	cols   int      // terminal width the frame clips its title, filter and footer lines to; 0 = unbounded
	typing bool     // the "/" filter is open: printable keys edit query instead of moving or quitting
	query  string   // filter text as typed
	terms  []string // query's lower-cased words; a row shows when it holds every one
	loose  bool     // a filtered view: its last row is not the separate trailing Back/Submit row
	many   bool     // the list is long enough (filterMinRows) for the footer to advertise "/ filter"
}

// match reports whether row i passes the filter; with no filter text every row does, without allocating.
func (s pickState) match(i int) bool {
	if len(s.terms) == 0 {
		return true
	}
	var row string
	if s.hay != nil {
		row = s.hay[i]
	} else {
		row = strings.ToLower(s.rows[i])
	}
	for _, t := range s.terms {
		if !strings.Contains(row, t) {
			return false
		}
	}
	return true
}

// step moves the cursor by dir past section-title rows and rows the filter hides; it stays put when none lies that way.
func (s pickState) step(dir int) pickState {
	for i := s.sel + dir; i >= 0 && i < len(s.rows); i += dir {
		if !s.heads[i] && s.match(i) {
			s.sel = i
			break
		}
	}
	return s
}

// handlePickKey is the pure transition: pickQuit on Esc/q/Ctrl-C/Left (same as Back), pickChoose on Enter.
func handlePickKey(s pickState, k Key) (pickState, int) {
	if s.typing {
		return handleFilterKey(s, k)
	}
	switch {
	case k.Kind == KeyCtrlC, k.Kind == KeyEsc, k.Kind == KeyLeft, k.Kind == KeyRune && k.Rune == 'q':
		return s, pickQuit
	case k.Kind == KeyDown, k.Kind == KeyRune && k.Rune == 'j':
		s = s.step(1)
	case k.Kind == KeyUp, k.Kind == KeyRune && k.Rune == 'k':
		s = s.step(-1)
	case k.Kind == KeyRune && k.Rune == '/':
		s.typing = true
	case k.Kind == KeyEnter:
		if len(s.rows) > 0 {
			return s, pickChoose
		}
	}
	return s, pickNone
}

// handleFilterKey is handlePickKey while the filter is open: Esc clears it and closes, Enter picks a shown row.
func handleFilterKey(s pickState, k Key) (pickState, int) {
	switch {
	case k.Kind == KeyCtrlC:
		return s, pickQuit
	case k.Kind == KeyEsc:
		return s.withQuery("", false), pickNone
	case k.Kind == KeyDown:
		s = s.step(1)
	case k.Kind == KeyUp:
		s = s.step(-1)
	case k.Kind == KeyEnter:
		if len(s.rows) > 0 && !s.heads[s.sel] && s.match(s.sel) {
			return s, pickChoose
		}
	case k.Kind == KeyBackspace:
		if q := []rune(s.query); len(q) > 0 {
			s = s.withQuery(string(q[:len(q)-1]), true)
		}
	case k.Kind == KeyRune && unicode.IsPrint(k.Rune):
		s = s.withQuery(s.query+string(k.Rune), true)
	}
	return s, pickNone
}

// withQuery sets the filter text and moves the cursor to the first shown row when its own row is hidden now.
func (s pickState) withQuery(q string, typing bool) pickState {
	s.query, s.typing, s.terms = q, typing, strings.Fields(strings.ToLower(q))
	if !s.heads[s.sel] && s.match(s.sel) {
		return s
	}
	for i := range s.rows {
		if !s.heads[i] && s.match(i) {
			s.sel = i
			break
		}
	}
	return s
}

// view is the rows the filter leaves, with a section title kept only above a surviving row; s itself when unfiltered.
func (s pickState) view() pickState {
	if len(s.terms) == 0 {
		return s
	}
	v := s
	v.rows, v.heads, v.sel, v.loose = nil, map[int]bool{}, 0, true
	head := -1
	for i, row := range s.rows {
		switch {
		case s.heads[i]:
			head = i
		case s.match(i):
			if head >= 0 {
				v.heads[len(v.rows)] = true
				v.rows, head = append(v.rows, s.rows[head]), -1
			}
			if i == s.sel {
				v.sel = len(v.rows)
			}
			v.rows = append(v.rows, row)
		}
	}
	return v
}

// rowCells is a row's cells: the name, then the detail split on 2+ space runs, a trailing " [..]" badge last.
func rowCells(r [2]string) []string {
	detail, badge := splitTrailBadge(oneLine(r[1]))
	cells := make([]string, 1, 4)
	cells[0] = oneLine(r[0])
	cells = appendMultiSpaceSplit(cells, detail)
	if badge != "" {
		cells = append(cells, badge)
	}
	return cells
}

func splitTrailBadge(detail string) (string, string) {
	if !strings.HasSuffix(detail, "]") {
		return detail, ""
	}
	best := -1 // leftmost " [" with no "]" between it and the end, like ` (\[[^\]]*\])$`
	for i := len(detail) - 2; i > 0; i-- {
		if detail[i] == ']' {
			break
		}
		if detail[i] == '[' && detail[i-1] == ' ' {
			best = i
		}
	}
	if best < 0 {
		return detail, ""
	}
	return detail[:best-1], detail[best:]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func appendMultiSpaceSplit(dst []string, s string) []string {
	start := 0
	for i := 0; i < len(s); {
		if !isSpaceByte(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && isSpaceByte(s[j]) {
			j++
		}
		if j-i >= 2 {
			dst = append(dst, s[start:i])
			start = j
		}
		i = j
	}
	return append(dst, s[start:])
}

// layoutRows lays rows out as columns shared by every row, each line at most width runes (0 = unbounded).
func layoutRows(rows [][2]string, width int) []string {
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = rowCells(r)
	}
	return table.Justify(cells, width, nil)
}

// Key-hint lines: the browse one offers "/ filter", the typing one says how to leave it.
const (
	filterMinRows = 8 // below this a menu fits the eye, so the "/ filter" hint would be noise; "/" still works
	hintPlain     = "↑/↓ move · enter select · esc back"
	hintBrowse    = "↑/↓ move · enter select · / filter · esc back"
	hintFilter    = "type to filter · ↑/↓ move · enter select · esc clear"
	noMatches     = "no matches"
)

var (
	hintPlainW  = textutil.DisplayWidth(hintPlain)
	hintBrowseW = textutil.DisplayWidth(hintBrowse)
	hintFilterW = textutil.DisplayWidth(hintFilter)
)

// hint is the footer line and its display width for the current mode.
func (s pickState) hint() (string, int) {
	if s.typing {
		return hintFilter, hintFilterW
	}
	if s.many {
		return hintBrowse, hintBrowseW
	}
	return hintPlain, hintPlainW
}

// clip cuts a title, filter or footer line to the frame's width so it never wraps; it is s itself when cols is 0.
func (s pickState) clip(line string) string {
	if s.cols <= 0 {
		return line
	}
	return textutil.TruncateCells(line, s.cols, "…")
}

// lineWidth is the display width a frame line of width w takes after clip.
func (s pickState) lineWidth(w int) int {
	if s.cols > 0 {
		return min(w, s.cols)
	}
	return w
}

// wrappedLines is the terminal rows a plain line of width n takes at cols columns; cols<=0 never wraps.
func wrappedLines(n, cols int) int {
	if cols <= 0 || n <= cols {
		return 1
	}
	return (n + cols - 1) / cols
}

// panelLines counts render's terminal lines by wrapped height, so a long row cannot make rewind undercount.
func panelLines(title string, s pickState, cols int) int {
	return panelLinesRange(title, s, 0, len(s.rows), cols)
}

func panelLinesRange(title string, s pickState, lo, hi, cols int) int {
	lines := 0
	switch {
	case title != "": // the filter line, when open, takes the blank line under the title
		lines += wrappedLines(s.lineWidth(textutil.DisplayWidth(title)), cols) + 1
	case s.typing: // a title-less (sectioned) panel opens on the filter line, else straight on its first section title
		lines++
	}
	for i := lo; i < hi; i++ {
		n := textutil.DisplayWidth(s.rows[i])
		if s.heads[i] {
			n = s.lineWidth(n) // render clips a heading to one line
		} else {
			n += 2
		}
		lines += wrappedLines(n, cols)
		if s.heads[i] && (i-lo > 0 || title == "") {
			lines++
		}
	}
	if hi-lo > 1 && !s.loose {
		lines++
	}
	if hi == lo && s.query != "" {
		lines++ // the "no matches" row
	}
	lines++
	_, w := s.hint()
	return lines + wrappedLines(s.lineWidth(w), cols)
}

// render draws the panel at the cursor without homing it; each line clears to its end so redraws leave no residue.
func render(title string, s pickState, color bool) string {
	var b strings.Builder
	writeTop(&b, title, s, color)
	for i, row := range s.rows {
		if i == len(s.rows)-1 && len(s.rows) > 1 && !s.loose {
			b.WriteString("\x1b[K\r\n")
		}
		if s.heads[i] {
			if i > 0 || title == "" {
				b.WriteString("\x1b[K\r\n")
			}
			b.WriteString(Accent(color, s.clip(row)))
			b.WriteString("\x1b[K\r\n")
			continue
		}
		prefix, line := "  ", ansi.Status(color, muteDetail(color, row))
		if i == s.sel {
			prefix = accentMark(color, "▸") + " "
			line = selected(color, ansi.Strip(row))
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\x1b[K\r\n")
	}
	if len(s.rows) == 0 && s.query != "" {
		b.WriteString("  ")
		b.WriteString(ansi.Style(color, ansi.Dim, noMatches))
		b.WriteString("\x1b[K\r\n")
	}
	h, _ := s.hint()
	b.WriteString("\x1b[K\r\n")
	b.WriteString(ansi.Style(color, ansi.Dim, s.clip(h)))
	b.WriteString("\x1b[K")
	return b.String()
}

// writeTop writes the title line and the line under it: the open filter, else a blank one under a title.
func writeTop(b *strings.Builder, title string, s pickState, color bool) {
	if title != "" {
		b.WriteString(Accent(color, s.clip(title)))
		b.WriteString("\x1b[K\r\n")
	}
	switch {
	case s.typing:
		b.WriteString(accentMark(color, "/"))
		b.WriteString(" ")
		b.WriteString(textutil.TruncateCells(s.query, max(s.cols-2, 1), "…")) // 2 cells for the "/ " prefix
		b.WriteString("\x1b[K\r\n")
	case title != "":
		b.WriteString("\x1b[K\r\n")
	}
}

// Pick runs an arrow-key single-choice panel; it returns the chosen index or -1, with a non-nil err off a terminal.
func Pick(title string, rows [][2]string, start int) (int, error) {
	return pickHeads(title, func(cols int) []string { return layoutRows(rows, cols) }, nil, start)
}

// pickHeads runs the picker over rows from lay(cols), re-laid per frame; the cursor skips section-title rows (heads).
func pickHeads(title string, lay func(cols int) []string, heads map[int]bool, start int) (idx int, err error) {
	t, err := openTerminal()
	if err != nil {
		return -1, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	s := pickState{rows: lay(0), heads: heads}
	s.hay, s.many = lowerAll(s.rows), len(s.rows) >= filterMinRows
	if n := len(s.rows); n > 0 {
		s.sel = min(max(start, 0), n-1)
	}
	if s.heads[s.sel] {
		s = s.step(1)
	}
	color := ansi.Enabled(os.Stdout)
	prev := 0 // lines the last frame drew: the next redraw rewinds exactly that many
	draw := func() {
		cols, h := t.Size() // per frame: a resize keeps the window and the rewind honest
		s.rows, s.cols = lay(max(cols-2, 0)), cols
		out, lines := frame(title, s, cols, h, color)
		if prev > 0 {
			fmt.Fprintf(os.Stdout, "\r\x1b[%dA\x1b[J", prev-1)
		}
		fmt.Fprint(os.Stdout, out)
		prev = lines
	}
	draw()
	defer func() { fmt.Fprintf(os.Stdout, "\r\x1b[%dA\x1b[J", prev-1) }()
	for k := range t.Keys() {
		var act int
		s, act = handlePickKey(s, k)
		switch act {
		case pickQuit:
			return -1, nil
		case pickChoose:
			return s.sel, nil
		}
		draw()
	}
	return -1, t.Err()
}

func lowerAll(rows []string) []string {
	low := make([]string, len(rows))
	for i, r := range rows {
		low[i] = strings.ToLower(r)
	}
	return low
}

// frame is one draw: the filtered rows windowed to the terminal height, rendered, and the lines that took.
func frame(title string, s pickState, cols, h int, color bool) (string, int) {
	f := fit(title, s.view(), cols, h)
	return render(title, f, color), panelLines(title, f, cols)
}

// fit windows rows around the cursor to h-1 lines; a taller panel would scroll out of the cursor-up rewind's reach.
func fit(title string, s pickState, cols, h int) pickState {
	if len(s.rows) == 0 || panelLines(title, s, cols) < h {
		return s
	}
	lo, hi := grow(len(s.rows), s.sel, func(lo, hi int) bool { return panelLinesRange(title, s, lo, hi, cols) < h })
	w := s
	w.rows, w.sel, w.heads = s.rows[lo:hi], s.sel-lo, map[int]bool{}
	for i := lo; i < hi; i++ {
		if s.heads[i] {
			w.heads[i-lo] = true
		}
	}
	return w
}

// PickBack is Pick with a trailing "Back" row; picking it, Esc, q or Ctrl-C all return -1.
func PickBack(title string, rows [][2]string, start int) (int, error) {
	withBack := make([][2]string, len(rows)+1)
	copy(withBack, rows)
	withBack[len(rows)] = [2]string{"Back", "return to the previous menu"}
	idx, err := Pick(title, withBack, start)
	if err != nil || idx == len(rows) {
		return -1, err
	}
	return idx, nil
}

// Section is one titled block of rows in PickSections.
type Section struct {
	Title string
	Rows  [][2]string
}

// sectionLines maps a sectioned panel: which display lines are titles, and the display line of each selectable row.
func sectionLines(sections []Section) (map[int]bool, []int) {
	heads := map[int]bool{}
	var pos []int
	line := 0
	for _, sec := range sections {
		heads[line] = true
		line++
		for range sec.Rows {
			pos = append(pos, line)
			line++
		}
	}
	return heads, append(pos, line)
}

// PickSections draws each section's rows under its title with shared columns; returns the selectable-row index or -1.
func PickSections(title string, sections []Section, trailing [2]string, start int) (int, error) {
	return pickSections("", sections, trailing, start)
}

// PickSectionsBack is PickSections under a page title with a trailing Back row; Back and Esc both return -1.
func PickSectionsBack(title string, sections []Section, start int) (int, error) {
	n := 0
	for _, sec := range sections {
		n += len(sec.Rows)
	}
	idx, err := pickSections(title, sections, [2]string{"Back", "return to the previous menu"}, start)
	if err != nil || idx == n {
		return -1, err
	}
	return idx, nil
}

func pickSections(title string, sections []Section, trailing [2]string, start int) (int, error) {
	var all [][2]string
	for _, sec := range sections {
		all = append(all, sec.Rows...)
	}
	all = append(all, trailing)
	heads, pos := sectionLines(sections)
	lay := func(cols int) []string {
		laid := layoutRows(all, cols)
		rows := make([]string, 0, pos[len(pos)-1]+1)
		for _, sec := range sections {
			rows = append(append(rows, sec.Title), laid[:len(sec.Rows)]...)
			laid = laid[len(sec.Rows):]
		}
		return append(rows, laid[0])
	}
	at := 0
	if start >= 0 && start < len(pos) {
		at = pos[start]
	}
	idx, err := pickHeads(title, lay, heads, at)
	if err != nil || idx < 0 {
		return -1, err
	}
	for n, p := range pos {
		if p == idx {
			return n, nil
		}
	}
	return -1, nil
}
