// Package dialog is the shell configure pages run on: a paged menu over ui/tui with Enter/Esc handling and shared rows.
package dialog

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/ui/tui"
)

// Shell is where a page's menus run: the arrow-key panel when TTY is set, else a numbered prompt on In and Out.
type Shell struct {
	Out io.Writer
	In  *bufio.Reader
	TTY bool
	// Paint, when set, colors the verdict tokens of each numbered-prompt line (the pages' status painter).
	Paint func(string) string
}

// Row is one menu row: Name, Detail, and what Enter does; Run reports true when the page is done.
type Row struct {
	Name, Detail string
	Run          func() (done bool)
}

// Pairs lays rows out as the (name, detail) pairs the panel renders.
func Pairs(rows []Row) [][2]string {
	out := make([][2]string, len(rows))
	for i, r := range rows {
		out[i] = [2]string{r.Name, r.Detail}
	}
	return out
}

// Mark flags the row holding the current value.
func Mark(current bool) string {
	if current {
		return " (current)"
	}
	return ""
}

// Pick returns the chosen row index, or -1 on Esc, Back or a blank answer; start is the TTY cursor row only.
func (s *Shell) Pick(title string, rows [][2]string, start ...int) (int, error) {
	if s.TTY {
		return tui.PickBack(title, rows, cmp.Or(start...))
	}
	return s.numbered(title, rows), nil
}

// PickPlain is Pick without the trailing Back row on a terminal, for a list that owns its own exit.
func (s *Shell) PickPlain(title string, rows [][2]string) (int, error) {
	if s.TTY {
		return tui.Pick(title, rows, 0)
	}
	return s.numbered(title, rows), nil
}

func (s *Shell) numbered(title string, rows [][2]string) int {
	s.say(title + "\n")
	for i, r := range rows {
		s.say(fmt.Sprintf("  %d) %-22s %s\n", i+1, r[0], r[1]))
	}
	fmt.Fprint(s.Out, "pick a number (blank = cancel): ")
	line, _ := s.In.ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimRight(line, "\r\n")))
	if err != nil || n < 1 || n > len(rows) {
		return -1
	}
	return n - 1
}

func (s *Shell) say(line string) {
	if s.Paint != nil {
		line = s.Paint(line)
	}
	fmt.Fprint(s.Out, line)
}

// Run shows rows (rebuilt each pass, cursor kept) and runs the one Enter picks, until a row reports done (true)
// or Esc/Back/an error dismisses the page (false).
func (s *Shell) Run(title string, rows func() []Row) bool {
	cur := 0
	for {
		rs := rows()
		idx, err := s.Pick(title, Pairs(rs), cur)
		if err != nil || idx < 0 || idx >= len(rs) {
			return false
		}
		cur = idx
		if rs[idx].Run != nil && rs[idx].Run() {
			return true
		}
	}
}
