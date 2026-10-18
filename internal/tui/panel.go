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
	rows   []string // "name  detail" rows, already column-aligned
	header string   // optional column header, drawn in place of the blank line under the title; never selectable
	sel    int
}

// handlePickKey is the pure transition: pickQuit on Esc/q/Ctrl-C (no choice), pickChoose on Enter.
func handlePickKey(s pickState, k Key) (pickState, int) {
	switch {
	case k.Kind == KeyCtrlC, k.Kind == KeyEsc, k.Kind == KeyRune && k.Rune == 'q':
		return s, pickQuit
	case k.Kind == KeyDown, k.Kind == KeyRune && k.Rune == 'j':
		if s.sel < len(s.rows)-1 {
			s.sel++
		}
	case k.Kind == KeyUp, k.Kind == KeyRune && k.Rune == 'k':
		if s.sel > 0 {
			s.sel--
		}
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

// panelLines is how many terminal lines render emits: title, blank, rows, blank, hint.
func panelLines(s pickState) int { return len(s.rows) + 4 }

// render draws the panel at the cursor: title, one row per option (▸ plus reverse video on the highlighted
// row), status tokens painted on the others, then the key hint. Each line clears to its end so a redraw
// over the previous frame leaves no residue. It never homes the cursor — the panel lives where the
// dialogue already is, below earlier output, not over the top of the screen.
func render(title string, s pickState, color bool) string {
	var b strings.Builder
	b.WriteString(ansi.Style(color, ansi.Bold+ansi.Cyan, title) + "\x1b[K\r\n")
	if s.header != "" {
		b.WriteString("  " + ansi.Style(color, ansi.Dim, s.header))
	}
	b.WriteString("\x1b[K\r\n")
	for i, row := range s.rows {
		prefix, line := "  ", ansi.Status(color, row)
		if i == s.sel {
			prefix = ansi.Style(color, ansi.Cyan, "▸") + " "
			line = ansi.Style(color, "\x1b[7m", row)
		}
		b.WriteString(prefix + line + "\x1b[K\r\n")
	}
	b.WriteString("\x1b[K\r\n" + ansi.Style(color, ansi.Dim, "↑↓/j k move · Enter pick · Esc cancel") + "\x1b[K")
	return b.String()
}

// rewind moves the cursor back to the panel's first line so the next render overdraws the last frame.
func rewind(s pickState) string { return fmt.Sprintf("\r\x1b[%dA", panelLines(s)-1) }

// erase removes the panel once a choice is made, so the dialogue continues with one result line instead
// of leaving the whole menu in the scrollback.
func erase(s pickState) string { return rewind(s) + "\x1b[J" }

// Pick runs an arrow-key single-choice panel titled title over rows (name, detail pairs), opening on
// start (clamped). Returns the chosen row index, or -1 on Esc/q/Ctrl-C. A non-error, non-nil err
// means stdin is not a terminal; the caller should fall back to a line prompt.
func Pick(title string, rows [][2]string, start int) (int, error) {
	return pickLaid(title, "", layoutRows(rows), start)
}

// pickLaid runs the picker over already-aligned rows, with an optional non-selectable header line.
func pickLaid(title, header string, rows []string, start int) (int, error) {
	t, err := Open()
	if err != nil {
		return -1, err
	}
	defer t.Close()
	s := pickState{rows: rows, header: header}
	if start >= 0 && start < len(s.rows) {
		s.sel = start
	}
	color := ansi.Enabled(os.Stdout)
	fmt.Fprint(os.Stdout, render(title, s, color))
	defer func() { fmt.Fprint(os.Stdout, erase(s)) }()
	for k := range t.Keys() {
		var act int
		s, act = handlePickKey(s, k)
		switch act {
		case pickQuit:
			return -1, nil
		case pickChoose:
			return s.sel, nil
		}
		fmt.Fprint(os.Stdout, rewind(s)+render(title, s, color))
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
