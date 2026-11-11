package tui

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

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
	rows  []string     // rows laid out at the current terminal width, re-laid on every draw
	heads map[int]bool // rows that are section titles: drawn after a blank line, never selectable
	sel   int
}

// step moves the cursor by dir past any section-title rows; it stays put when nothing selectable lies that way.
func (s pickState) step(dir int) pickState {
	for i := s.sel + dir; i >= 0 && i < len(s.rows); i += dir {
		if !s.heads[i] {
			s.sel = i
			break
		}
	}
	return s
}

// handlePickKey is the pure transition: pickQuit on Esc/q/Ctrl-C/Left (no choice, same as Back), pickChoose
// on Enter. Left is the same "go back" signal as Back/Esc at this level: a submenu's caller is the top
// menu, so pickQuit there already is "back to the first page".
func handlePickKey(s pickState, k Key) (pickState, int) {
	switch {
	case k.Kind == KeyCtrlC, k.Kind == KeyEsc, k.Kind == KeyLeft, k.Kind == KeyRune && k.Rune == 'q':
		return s, pickQuit
	case k.Kind == KeyDown, k.Kind == KeyRune && k.Rune == 'j':
		s = s.step(1)
	case k.Kind == KeyUp, k.Kind == KeyRune && k.Rune == 'k':
		s = s.step(-1)
	case k.Kind == KeyEnter:
		if len(s.rows) > 0 {
			return s, pickChoose
		}
	}
	return s, pickNone
}

var (
	detailSplit = regexp.MustCompile(`\s{2,}`)
	trailBadge  = regexp.MustCompile(` (\[[^\]]*\])$`)
)

// rowCells is a row's cells: the name, then the detail split on 2+ space runs, a trailing " [..]" badge last.
func rowCells(r [2]string) []string {
	detail := oneLine(r[1])
	badge := ""
	if m := trailBadge.FindStringSubmatchIndex(detail); m != nil {
		badge, detail = detail[m[2]:m[3]], detail[:m[0]]
	}
	cells := append([]string{oneLine(r[0])}, detailSplit.Split(detail, -1)...)
	if badge != "" {
		cells = append(cells, badge)
	}
	return cells
}

// layoutRows lays rows out as columns shared by every row, each line at most width runes (0 = unbounded).
func layoutRows(rows [][2]string, width int) []string {
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = rowCells(r)
	}
	return table.Justify(cells, width, nil)
}

// hintText is the key-hint line render draws last; panelLines counts it by the same literal.
const hintText = "↑/↓ to navigate · enter to select · esc to go back"

// wrappedLines is how many terminal rows a plain (unstyled) line of length n occupies at cols columns -
// 1 below cols, ceil(n/cols) at or above it. cols<=0 (size query failed) never wraps.
func wrappedLines(n, cols int) int {
	if cols <= 0 || n <= cols {
		return 1
	}
	return (n + cols - 1) / cols
}

// panelLines is how many terminal lines render emits at cols columns: title, blank, one per row
// (each 2 cols wider for its marker/space prefix), blank, hint - each counted by its wrapped height, not
// 1, so a row too long for a narrow terminal doesn't make rewind undercount and leave stale frames behind.
func panelLines(title string, s pickState, cols int) int {
	lines := 0
	if title != "" { // a title-less (sectioned) panel opens straight on its first section title
		lines += wrappedLines(len([]rune(title)), cols) + 1
	}
	for i, row := range s.rows {
		n := len([]rune(row))
		if !s.heads[i] {
			n += 2 // the marker/space prefix; section titles are drawn bare
		}
		lines += wrappedLines(n, cols)
		if s.heads[i] && (i > 0 || title == "") {
			lines++ // the blank line above a section title (the page title's own blank covers the first)
		}
	}
	if len(s.rows) > 1 {
		lines++ // blank line separating the trailing Submit/Back row from the rest
	}
	lines++ // blank line before the hint
	lines += wrappedLines(len([]rune(hintText)), cols)
	return lines
}

// render draws the panel at the cursor: title, one row per option (▸ plus reverse video on the highlighted
// row), status tokens painted on the others, then the key hint. Each line clears to its end so a redraw
// over the previous frame leaves no residue. It never homes the cursor — the panel lives where the
// dialogue already is, below earlier output, not over the top of the screen.
func render(title string, s pickState, color bool) string {
	var b strings.Builder
	if title != "" {
		b.WriteString(Accent(color, title) + "\x1b[K\r\n\x1b[K\r\n")
	}
	for i, row := range s.rows {
		if i == len(s.rows)-1 && len(s.rows) > 1 {
			b.WriteString("\x1b[K\r\n") // every menu's trailing Submit/Back row sits one blank line below the rest
		}
		if s.heads[i] {
			if i > 0 || title == "" { // the page title already ends in a blank line
				b.WriteString("\x1b[K\r\n")
			}
			b.WriteString(Accent(color, row) + "\x1b[K\r\n")
			continue
		}
		prefix, line := "  ", ansi.Status(color, muteDetail(color, row))
		if i == s.sel {
			prefix = accentMark(color, "▸") + " "
			line = selected(color, ansi.Strip(row))
		}
		b.WriteString(prefix + line + "\x1b[K\r\n")
	}
	b.WriteString("\x1b[K\r\n" + ansi.Style(color, ansi.Dim, hintText) + "\x1b[K")
	return b.String()
}

// Pick runs an arrow-key single-choice panel titled title over rows (name, detail pairs), opening on
// start (clamped). Returns the chosen row index, or -1 on Esc/q/Ctrl-C. A non-error, non-nil err
// means stdin is not a terminal; the caller should fall back to a line prompt.
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
	if n := len(s.rows); n > 0 {
		s.sel = min(max(start, 0), n-1) // a start outside the rows clamps to the nearest row
	}
	if s.heads[s.sel] {
		s = s.step(1)
	}
	color := ansi.Enabled(os.Stdout)
	prev := 0 // lines the last frame drew: the next redraw rewinds exactly that many
	draw := func() {
		cols, h := t.Size()          // per frame: a resize keeps the window and the rewind honest
		s.rows = lay(max(cols-2, 0)) // 2 = the marker prefix
		f := fit(title, s, cols, h)
		if prev > 0 {
			fmt.Fprintf(os.Stdout, "\r\x1b[%dA\x1b[J", prev-1)
		}
		fmt.Fprint(os.Stdout, render(title, f, color))
		prev = panelLines(title, f, cols)
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

// fit windows s's rows around the cursor so the panel fits h-1 terminal lines; a panel taller than the screen
// scrolls its top out of reach of the cursor-up rewind, and every redraw then leaves a stale copy above.
func fit(title string, s pickState, cols, h int) pickState {
	if len(s.rows) == 0 || panelLines(title, s, cols) < h {
		return s
	}
	win := func(lo, hi int) pickState {
		w := pickState{rows: s.rows[lo:hi], sel: s.sel - lo, heads: map[int]bool{}}
		for i := lo; i < hi; i++ {
			if s.heads[i] {
				w.heads[i-lo] = true
			}
		}
		return w
	}
	lo, hi := grow(len(s.rows), s.sel, func(lo, hi int) bool { return panelLines(title, win(lo, hi), cols) < h })
	return win(lo, hi)
}

// PickBack is Pick with a trailing "Back" row appended; picking it (or Esc/q/Ctrl-C) both return -1, so a
// caller with a menu loop re-shows the enclosing menu on -1 and a caller without one keeps its existing
// "kept, nothing changed" path unmodified.
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
		heads[line] = true // every section, the first included: a blank line, then its title
		line++
		for range sec.Rows {
			pos = append(pos, line)
			line++
		}
	}
	return heads, append(pos, line)
}

// PickSections draws every section's rows under its own title (each section carries one; title is unused), with
// columns shared across all sections, then trailing after a blank line. It returns the index among selectable rows
// in order (trailing = the last), or -1 on Esc/q/Ctrl-C/Left; start is in the same numbering.
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
