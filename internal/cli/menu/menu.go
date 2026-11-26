// Package menu is the configure dialog: its sections, agent pages and key prompts, plus the row layout they share.
package menu

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/parallel"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// row is one selectable row: Name, then State and Check in aligned columns, and what picking it does with the dialog.
type row struct {
	Name, State, Check string
	Run                func(*dialog)
}

// panicRow is the row text of a contained panic; the value may hold a secret, so it and the stack go to stderr only.
const panicRow = "error: internal panic (stack on stderr)"

// panicLine is err's one-line row text; a contained panic prints once to stderr, before the menu opens.
func panicLine(err error) string {
	var pe *parallel.PanicError
	if !errors.As(err, &pe) {
		return err.Error()
	}
	fmt.Fprintln(os.Stderr, pe)
	return panicRow
}

// fanOut keeps a panicking builder from hiding its row or its siblings.
func fanOut(n int, fn func(i int), fail func(i int, err error)) {
	for i, err := range parallel.All(n, func(i int) error { fn(i); return nil }) {
		if err != nil {
			fail(i, err)
		}
	}
}

// block is a titled group of rows.
type block struct {
	Title string
	Rows  []row
}

// heading is a section's title line.
func heading(title string) string { return "== " + title + " ==" }

// enterRow is a page's last row: Enter leaves the page, asking first when it has unsaved changes.
func enterRow(detail string) [2]string { return [2]string{"Enter", detail} }

// bracket is a row's trailing state: [yes] or [no].
func bracket(ok bool, yes, no string) string {
	if ok {
		return "[" + yes + "]"
	}
	return "[" + no + "]"
}

// checked is a bracketed value that paints green when valid and dim otherwise (only where Status paints the row).
func checked(valid bool, s string) string {
	open := ansi.SpanOff
	if valid {
		open = ansi.SpanOK
	}
	return "[" + open + s + ansi.SpanEnd + "]"
}

// padTo pads s to n cells; an s already that wide gets a two-space gap instead.
func padTo(s string, n int) string {
	if textutil.DisplayWidth(s) >= n {
		return s + "  "
	}
	return textutil.PadCells(s, n, false)
}

// withChecks lays out name rows whose detail is two aligned columns: the mode, then the check ("verified" is green).
func withChecks(names []string, cols [][2]string) [][2]string {
	width := 0
	for _, c := range cols {
		width = max(width, textutil.DisplayWidth(c[0]))
	}
	rows := make([][2]string, len(names))
	for i, c := range cols {
		pad := strings.Repeat(" ", width-textutil.DisplayWidth(c[0]))
		rows[i] = [2]string{names[i], strings.TrimRight(c[0]+pad+"  "+c[1], " ")}
	}
	return rows
}

// rowsOf lays rows out with withChecks and returns what each one runs, in row order.
func rowsOf(rs []row) ([][2]string, []func(*dialog)) {
	names := make([]string, len(rs))
	cols := make([][2]string, len(rs))
	acts := make([]func(*dialog), len(rs))
	for i, r := range rs {
		names[i], cols[i], acts[i] = r.Name, [2]string{r.State, r.Check}, r.Run
	}
	return withChecks(names, cols), acts
}

// sections lays each block out on its own and returns the actions of every block, in row order.
func sections(blocks []block) ([]tui.Section, []func(*dialog)) {
	var sections []tui.Section
	var actions []func(*dialog)
	for _, b := range blocks {
		r, a := rowsOf(b.Rows)
		sections = append(sections, tui.Section{Title: heading(b.Title), Rows: r})
		actions = append(actions, a...)
	}
	return sections, actions
}

// shell is where a page's menus run: the arrow-key panel when TTY is set, else a numbered prompt on In and Out.
type shell struct {
	Out io.Writer
	In  *bufio.Reader
	TTY bool
	// Paint, when set, colors the verdict tokens of each numbered-prompt line (the pages' status painter).
	Paint func(string) string
}

// shellRow is one menu row: Name, Detail, and what Enter does; Run reports true when the page is done.
type shellRow struct {
	Name, Detail string
	Run          func() (done bool)
}

// pairs lays rows out as the (name, detail) pairs the panel renders.
func pairs(rows []shellRow) [][2]string {
	out := make([][2]string, len(rows))
	for i, r := range rows {
		out[i] = [2]string{r.Name, r.Detail}
	}
	return out
}

// mark flags the row holding the current value.
func mark(current bool) string {
	if current {
		return " (current)"
	}
	return ""
}

// crumb is a page title as the panel's breadcrumb: "configure", then each " — " part as a level.
func crumb(title string) string {
	if title == "" {
		return ""
	}
	return "configure › " + strings.ReplaceAll(title, " — ", " › ")
}

// Pick returns the chosen row index, or -1 on Esc, Back or a blank answer; start is the TTY cursor row only.
func (s *shell) Pick(title string, rows [][2]string, start ...int) (int, error) {
	if s.TTY {
		return tui.PickBack(crumb(title), rows, cmp.Or(start...))
	}
	return s.numbered(title, rows), nil
}

// pickPlain is Pick without the trailing Back row on a terminal, for a list that owns its own exit.
func (s *shell) pickPlain(title string, rows [][2]string) (int, error) {
	if s.TTY {
		return tui.Pick(crumb(title), rows, 0)
	}
	return s.numbered(title, rows), nil
}

func (s *shell) numbered(title string, rows [][2]string) int {
	s.say(title + "\n")
	for i, r := range rows {
		s.say(fmt.Sprintf("  %d) %-22s %s\n", i+1, r[0], r[1]))
	}
	fmt.Fprint(s.Out, "pick a number (blank = cancel): ")
	line, _ := s.In.ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(rows) {
		return -1
	}
	return n - 1
}

func (s *shell) say(line string) {
	if s.Paint != nil {
		line = s.Paint(line)
	}
	fmt.Fprint(s.Out, line)
}

// Confirm asks a [Y/n] question: tui.Confirm on the terminal, a line from In otherwise.
func (s *shell) Confirm(question string) bool {
	if s.TTY {
		return tui.Confirm(question, true)
	}
	fmt.Fprintf(s.Out, "%s [Y/n] ", question)
	return yes(s.In)
}

// lineReader is tui's shared stdin reader, so Line and tui.Confirm/Secret never drop each other's look-ahead.
var lineReader = tui.Stdin

// Line reads one visible line: straight from the terminal like tui.Confirm, from In otherwise.
func (s *shell) Line(prompt string) string {
	fmt.Fprint(s.Out, prompt)
	if s.TTY {
		return readLine(lineReader())
	}
	return readLine(s.In)
}

// readLine is one line from br without its line ending; a read error yields what arrived before it.
func readLine(br *bufio.Reader) string {
	line, _ := br.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// yes reads a [Y/n] answer: blank or y/Y is yes; end of input declines, so a dry pipe never starts a login or paste.
func yes(br *bufio.Reader) bool {
	raw, err := br.ReadString('\n')
	return (err == nil || raw != "") && (strings.TrimSpace(raw) == "" || tui.ParseYesNo(raw, false))
}

// confirmTerminal asks question by keypress on a terminal (Enter takes def, Esc is no); non-terminal runs proceed.
func confirmTerminal(question string, def bool) bool {
	if !tui.Interactive() {
		return true
	}
	return tui.ConfirmKey(question+"?", def)
}

// Run shows rows (rebuilt each pass, cursor kept) and runs the one Enter picks.
func (s *shell) Run(title string, rows func() []shellRow) bool {
	cur := 0
	for {
		rs := rows()
		idx, err := s.Pick(title, pairs(rs), cur)
		if err != nil || idx < 0 || idx >= len(rs) {
			return false
		}
		cur = idx
		if rs[idx].Run != nil && rs[idx].Run() {
			return true
		}
	}
}
