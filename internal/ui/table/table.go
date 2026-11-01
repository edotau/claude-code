// Package table renders plain-text, column-aligned tables for list views (vault list, usage, quotas).
package table

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ui/ansi"
)

// Truncate clips a plain (unpainted) line to width runes, ending in "…"; width <= 0 means no cap.
func Truncate(s string, width int) string {
	r := []rune(s)
	if width <= 0 || len(r) <= width {
		return s
	}
	if width <= 1 {
		return string(r[:width])
	}
	return string(r[:width-1]) + "…"
}

// Render lays out a header plus rows as two-space-indented, column-aligned, width-truncated lines.
// colMax caps each column's cells (0 or a missing entry = no cap).
func Render(head []string, colMax []int, rows [][]string, width int) string {
	rows = slices.Clone(rows)
	for i, r := range rows {
		r = slices.Clone(r)
		for c := range r {
			if c < len(colMax) && colMax[c] > 0 {
				r[c] = Truncate(r[c], colMax[c])
			}
		}
		rows[i] = r
	}
	ws := make([]int, len(head))
	for _, r := range append([][]string{head}, rows...) {
		for i, c := range r {
			ws[i] = max(ws[i], utf8.RuneCountInString(c))
		}
	}
	var b strings.Builder
	for _, r := range append([][]string{head}, rows...) {
		line := " "
		var lineSb strings.Builder
		for i, c := range r {
			lineSb.WriteByte(' ')
			lineSb.WriteString(c)
			if i < len(r)-1 {
				lineSb.WriteString(strings.Repeat(" ", ws[i]-utf8.RuneCountInString(c)+1))
			}
		}
		line += lineSb.String()
		b.WriteString(Truncate(strings.TrimRight(line, " "), width))
		b.WriteByte('\n')
	}
	return b.String()
}

// Styled is Render with a style code per row (painted after alignment, so padding sees plain text).
func Styled(head []string, colMax []int, rows [][]string, width int, codes []string, color bool) string {
	lines := strings.Split(strings.TrimRight(Render(head, colMax, rows, width), "\n"), "\n")
	for i, l := range lines {
		code := ansi.Dim
		if i > 0 {
			code = codes[i-1]
		}
		lines[i] = ansi.Style(color && code != "", code, l)
	}
	return strings.Join(lines, "\n") + "\n"
}

// DropEmptyCols removes every column past the first that is empty in all rows, trimming head and colMax with it.
func DropEmptyCols(head []string, colMax []int, rows [][]string) ([]string, []int, [][]string) {
	var keep []int
	for c := range head {
		if c == 0 || slices.ContainsFunc(rows, func(r []string) bool { return r[c] != "" }) {
			keep = append(keep, c)
		}
	}
	pick := func(r []string) []string {
		out := make([]string, len(keep))
		for i, c := range keep {
			out[i] = r[c]
		}
		return out
	}
	cols := make([]int, len(keep))
	outRows := make([][]string, len(rows))
	for i, c := range keep {
		cols[i] = colMax[c]
	}
	for i, r := range rows {
		outRows[i] = pick(r)
	}
	return pick(head), cols, outRows
}
