package tui

import (
	"fmt"
	"os"
	"strings"
)

// colorEnabled follows the project's https://no-color.org convention: on only when stdout is a
// terminal and NO_COLOR is unset (the same gate internal/cli's usage() applies).
func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

const (
	pickNone = iota
	pickChoose
	pickQuit
)

// pickState is the panel's pure state, separated from I/O so key handling tests without a TTY.
type pickState struct {
	rows []string // "name  detail" rows, already column-aligned
	sel  int
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

// render draws the title, one row per option (▸ plus reverse video on the highlighted row when
// color is on), and the key hint; redraws in place via cursor-home + clear-to-end-of-line.
func render(title string, s pickState, color bool) string {
	var b strings.Builder
	b.WriteString("\x1b[H" + title + "\x1b[K\r\n\x1b[K\r\n")
	for i, row := range s.rows {
		prefix, line := "  ", row
		if i == s.sel {
			prefix = "▸ "
			if color {
				line = "\x1b[7m" + row + "\x1b[0m"
			}
		}
		b.WriteString(prefix + line + "\x1b[K\r\n")
	}
	b.WriteString("\x1b[K\r\n↑↓/j k move · Enter pick · Esc cancel\x1b[K")
	return b.String()
}

// Pick runs an arrow-key single-choice panel titled title over rows (name, detail pairs), opening on
// start (clamped). Returns the chosen row index, or -1 on Esc/q/Ctrl-C. A non-error, non-nil err
// means stdin is not a terminal; the caller should fall back to a line prompt.
func Pick(title string, rows [][2]string, start int) (int, error) {
	t, err := Open()
	if err != nil {
		return -1, err
	}
	defer t.Close()
	s := pickState{rows: layoutRows(rows)}
	if start >= 0 && start < len(s.rows) {
		s.sel = start
	}
	color := colorEnabled()
	fmt.Fprint(os.Stdout, render(title, s, color))
	for k := range t.Keys() {
		var act int
		s, act = handlePickKey(s, k)
		switch act {
		case pickQuit:
			return -1, nil
		case pickChoose:
			return s.sel, nil
		}
		fmt.Fprint(os.Stdout, render(title, s, color))
	}
	return -1, nil
}
