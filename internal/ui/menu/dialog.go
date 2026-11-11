// Shell is the shell configure pages run on: a paged menu over ui/tui with Enter/Esc handling and shared rows.

package menu

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
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

// ShellRow is one menu row: Name, Detail, and what Enter does; Run reports true when the page is done.
type ShellRow struct {
	Name, Detail string
	Run          func() (done bool)
}

// Pairs lays rows out as the (name, detail) pairs the panel renders.
func Pairs(rows []ShellRow) [][2]string {
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

// Confirm asks a [Y/n] question: tui.Confirm on the terminal, a line from In otherwise.
func (s *Shell) Confirm(question string) bool {
	if s.TTY {
		return tui.Confirm(question, true)
	}
	fmt.Fprintf(s.Out, "%s [Y/n] ", question)
	return Yes(s.In)
}

// lineReader is tui's shared stdin reader, so Line and tui.Confirm/Secret never drop each other's look-ahead.
var lineReader = tui.Stdin

// Line reads one visible line: straight from the terminal like tui.Confirm, from In otherwise.
func (s *Shell) Line(prompt string) string {
	fmt.Fprint(s.Out, prompt)
	if s.TTY {
		return ReadLine(lineReader())
	}
	return ReadLine(s.In)
}

// ReadLine is one line from br without its line ending; a read error yields what arrived before it.
func ReadLine(br *bufio.Reader) string {
	line, _ := br.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// Yes reads a [Y/n] answer: blank or y/Y is yes; end of input declines, so a dry pipe never starts a login or paste.
func Yes(br *bufio.Reader) bool {
	raw, err := br.ReadString('\n')
	if err != nil && raw == "" {
		return false
	}
	line := strings.ToLower(strings.TrimSpace(raw))
	return line == "" || strings.HasPrefix(line, "y")
}

// ConfirmTerminal asks question by keypress on a terminal (Enter takes def, Esc is no); non-terminal runs proceed.
func ConfirmTerminal(question string, def bool) bool {
	if !proc.IsTerminal(os.Stdin) {
		return true
	}
	return tui.ConfirmKey(question+"?", def)
}

// Run shows rows (rebuilt each pass, cursor kept) and runs the one Enter picks, until a row reports done (true)
// or Esc/Back/an error dismisses the page (false).
func (s *Shell) Run(title string, rows func() []ShellRow) bool {
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
