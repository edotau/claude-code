// Package menu lays out terminal menu rows: aligned columns, state brackets, section titles and the Enter row.
package menu

import (
	"strings"

	"github.com/edotau/claude-code/internal/ui/tui"
)

// Row is one selectable row: Name, then State and Check in aligned columns, and what picking it does with an A.
type Row[A any] struct {
	Name, State, Check string
	Run                func(A)
}

// Block is a titled group of rows.
type Block[A any] struct {
	Title string
	Rows  []Row[A]
}

// Heading is a section's title line.
func Heading(title string) string { return "== " + title + " ==" }

// EnterRow is a page's last row: Enter leaves the page, asking first when it has unsaved changes.
func EnterRow(detail string) [2]string { return [2]string{"Enter", detail} }

// Bracket is a row's trailing state: [yes] or [no].
func Bracket(ok bool, yes, no string) string {
	if ok {
		return "[" + yes + "]"
	}
	return "[" + no + "]"
}

// PadTo pads s to n columns of runes; an s already that wide gets a two-space gap instead.
func PadTo(s string, n int) string {
	if len([]rune(s)) >= n {
		return s + "  "
	}
	return s + strings.Repeat(" ", n-len([]rune(s)))
}

// WithChecks lays out name rows whose detail is two aligned columns: the mode, then the check ("verified" is green).
func WithChecks(names []string, cols [][2]string) [][2]string {
	width := 0
	for _, c := range cols {
		width = max(width, len([]rune(c[0])))
	}
	rows := make([][2]string, len(names))
	for i, c := range cols {
		pad := strings.Repeat(" ", width-len([]rune(c[0])))
		rows[i] = [2]string{names[i], strings.TrimRight(c[0]+pad+"  "+c[1], " ")}
	}
	return rows
}

// RowsOf lays rows out with WithChecks and returns what each one runs, in row order.
func RowsOf[A any](rs []Row[A]) ([][2]string, []func(A)) {
	names := make([]string, len(rs))
	cols := make([][2]string, len(rs))
	acts := make([]func(A), len(rs))
	for i, r := range rs {
		names[i], cols[i], acts[i] = r.Name, [2]string{r.State, r.Check}, r.Run
	}
	return WithChecks(names, cols), acts
}

// Sections lays each block out on its own and returns the actions of every block, in row order.
func Sections[A any](blocks []Block[A]) ([]tui.Section, []func(A)) {
	var sections []tui.Section
	var actions []func(A)
	for _, b := range blocks {
		r, a := RowsOf(b.Rows)
		sections = append(sections, tui.Section{Title: Heading(b.Title), Rows: r})
		actions = append(actions, a...)
	}
	return sections, actions
}
