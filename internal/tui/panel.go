package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/ansi"
)

const (
	pickNone = iota
	pickChoose
	pickQuit
)

// pickState is the panel's pure state, separated from I/O so key handling tests without a TTY.
type pickState struct {
	rows   []string     // "name  detail" rows, already column-aligned
	header string       // optional column header, drawn in place of the blank line under the title; never selectable
	heads  map[int]bool // rows that are section titles: drawn after a blank line, never selectable
	sel    int
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

// layoutRows pads every name to the widest, so every row's detail column lines up.
func layoutRows(rows [][2]string) []string {
	width := 0
	for _, r := range rows {
		if n := len(r[0]); n > width {
			width = n
		}
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%-*s  %s", width, r[0], r[1])
	}
	return out
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

// panelLines is how many terminal lines render emits at cols columns: title, blank/header, one per row
// (each 2 cols wider for its marker/space prefix), blank, hint - each counted by its wrapped height, not
// 1, so a row too long for a narrow terminal doesn't make rewind undercount and leave stale frames behind.
func panelLines(title string, s pickState, cols int) int {
	lines := 0
	if title != "" { // a title-less (sectioned) panel opens straight on its first section title
		lines += wrappedLines(len([]rune(title)), cols)
		if s.header != "" {
			lines += wrappedLines(2+len([]rune(s.header)), cols)
		} else {
			lines++
		}
	}
	for i, row := range s.rows {
		lines += wrappedLines(2+len([]rune(row)), cols)
		if s.heads[i] {
			lines++ // the blank line above a section title
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
		b.WriteString(ansi.Style(color, ansi.Bold+ansi.Cyan, title) + "\x1b[K\r\n")
		if s.header != "" {
			b.WriteString("  " + ansi.Style(color, ansi.Dim, s.header))
		}
		b.WriteString("\x1b[K\r\n")
	}
	for i, row := range s.rows {
		if i == len(s.rows)-1 && len(s.rows) > 1 {
			b.WriteString("\x1b[K\r\n") // every menu's trailing Submit/Back row sits one blank line below the rest
		}
		if s.heads[i] {
			b.WriteString("\x1b[K\r\n" + ansi.Style(color, ansi.Bold+ansi.Cyan, row) + "\x1b[K\r\n")
			continue
		}
		prefix, line := "  ", ansi.Status(color, row)
		if i == s.sel {
			prefix = ansi.Style(color, ansi.Cyan, "▸") + " "
			line = ansi.Style(color, "\x1b[7m", row)
		}
		b.WriteString(prefix + line + "\x1b[K\r\n")
	}
	b.WriteString("\x1b[K\r\n" + ansi.Style(color, ansi.Dim, hintText) + "\x1b[K")
	return b.String()
}

// rewind moves the cursor back to the panel's first line so the next render overdraws the last frame.
func rewind(title string, s pickState, cols int) string {
	return fmt.Sprintf("\r\x1b[%dA", panelLines(title, s, cols)-1)
}

// erase removes the panel once a choice is made, so the dialogue continues with one result line instead
// of leaving the whole menu in the scrollback.
func erase(title string, s pickState, cols int) string { return rewind(title, s, cols) + "\x1b[J" }

// Pick runs an arrow-key single-choice panel titled title over rows (name, detail pairs), opening on
// start (clamped). Returns the chosen row index, or -1 on Esc/q/Ctrl-C. A non-error, non-nil err
// means stdin is not a terminal; the caller should fall back to a line prompt.
func Pick(title string, rows [][2]string, start int) (int, error) {
	return pickLaid(title, "", layoutRows(rows), start)
}

// pickLaid runs the picker over already-aligned rows, with an optional non-selectable header line.
func pickLaid(title, header string, rows []string, start int) (int, error) {
	return pickHeads(title, header, rows, nil, start)
}

// pickHeads is pickLaid with section-title rows (heads); the cursor never lands on one.
func pickHeads(title, header string, rows []string, heads map[int]bool, start int) (int, error) {
	t, err := Open()
	if err != nil {
		return -1, err
	}
	defer t.Close()
	s := pickState{rows: rows, header: header, heads: heads}
	if start >= 0 && start < len(s.rows) {
		s.sel = start
	}
	if s.heads[s.sel] {
		s = s.step(1)
	}
	color := ansi.Enabled(os.Stdout)
	cols, _ := t.Size() // re-queried each frame below: a mid-session resize keeps rewind's count honest
	fmt.Fprint(os.Stdout, render(title, s, color))
	defer func() { cols, _ = t.Size(); fmt.Fprint(os.Stdout, erase(title, s, cols)) }()
	for k := range t.Keys() {
		var act int
		s, act = handlePickKey(s, k)
		switch act {
		case pickQuit:
			return -1, nil
		case pickChoose:
			return s.sel, nil
		}
		fmt.Fprint(os.Stdout, rewind(title, s, cols)+render(title, s, color))
		cols, _ = t.Size()
	}
	return -1, nil
}

// PickTable is PickBack with a column header: header[0] heads the name column, header[1] the detail column,
// aligned with the rows and never selectable.
func PickTable(title string, header [2]string, rows [][2]string, start int) (int, error) {
	withBack := append(append([][2]string{}, rows...), [2]string{"Back", "return to the previous menu"})
	laid := layoutRows(append([][2]string{header}, withBack...))
	idx, err := pickLaid(title, laid[0], laid[1:], start)
	if err != nil || idx == len(rows) {
		return -1, err
	}
	return idx, nil
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

// PickSections draws every section's rows under its own title (title itself is unused: each section carries one) (columns aligned across all of them), then
// trailing after a blank line. It returns the index among selectable rows in order (trailing = the last),
// or -1 on Esc/q/Ctrl-C/Left; start is in the same numbering.
func PickSections(title string, sections []Section, trailing [2]string, start int) (int, error) {
	var all [][2]string
	for _, sec := range sections {
		all = append(all, sec.Rows...)
	}
	laid := layoutRows(append(all, trailing))
	var rows []string
	heads := map[int]bool{}
	var pos []int // display index of each selectable row
	k := 0
	for _, sec := range sections {
		heads[len(rows)] = true // every section, the first included: a blank line, then its title
		rows = append(rows, sec.Title)
		for range sec.Rows {
			pos = append(pos, len(rows))
			rows = append(rows, laid[k])
			k++
		}
	}
	pos = append(pos, len(rows))
	rows = append(rows, laid[k])
	at := 0
	if start >= 0 && start < len(pos) {
		at = pos[start]
	}
	idx, err := pickHeads("", "", rows, heads, at)
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
