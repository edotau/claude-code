// Package table renders plain-text, column-aligned tables for list views (vault ls, usage, quotas).
package table

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// Truncate clips a plain (unpainted) line to width runes, ending in "…"; width <= 0 means no cap.
func Truncate(s string, width int) string {
	if width <= 0 {
		return s
	}
	return textutil.Truncate(s, width)
}

// TruncateMiddle clips s to width runes keeping both ends, so names that share a long prefix stay distinct.
func TruncateMiddle(s string, width int) string {
	r := []rune(s)
	if width <= 2 || len(r) <= width {
		return Truncate(s, width)
	}
	tail := (width - 1) / 2
	return string(r[:width-1-tail]) + "…" + string(r[len(r)-tail:])
}

const (
	maxGap   = 8  // widest gap between columns when the page has room to spare
	minColW  = 4  // a column never shrinks below this to fit the terminal
	spreadTo = 80 // the width a table spreads its gaps toward
)

// shrinkWidths returns ws shrunk (widest column first, down to minColW) until the columns plus 2-space gaps fit width.
func shrinkWidths(ws []int, width int) []int {
	ws = slices.Clone(ws)
	for width > 0 && sumInts(ws)+2*(len(ws)-1) > width {
		w := slices.Index(ws, slices.Max(ws))
		if ws[w] <= minColW {
			break
		}
		ws[w]--
	}
	return ws
}

func sumInts(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

// Justify lays rows out as plain column-aligned lines (no indent) no wider than width (<= 0 = unknown, no cap).
// A short row's last cell spans the remaining columns and does not size its column; clip shortens a cell (nil = Truncate).
func Justify(rows [][]string, width int, clip func(string, int) string) []string {
	if clip == nil {
		clip = Truncate
	}
	n := 0
	for _, r := range rows {
		n = max(n, len(r))
	}
	ws := shrinkWidths(columnWidths(rows, n), width)
	gap := columnGap(ws, n, width)
	out := make([]string, len(rows))
	for k, r := range rows {
		out[k] = justifyRow(r, n, ws, gap, width, clip)
	}
	return out
}

// spansRest reports whether cell i is the last of a short row (fewer than n cells), which spans the remaining columns.
func spansRest(r []string, n, i int) bool { return len(r) < n && i == len(r)-1 }

// columnWidths sizes each of n columns to its widest cell, ignoring the spanning cells of short rows.
func columnWidths(rows [][]string, n int) []int {
	ws := make([]int, n)
	for _, r := range rows {
		for i, c := range r {
			if !spansRest(r, n, i) {
				ws[i] = max(ws[i], utf8.RuneCountInString(c))
			}
		}
	}
	return ws
}

// columnGap spreads leftover room (up to spreadTo columns) across the n-1 gaps, at least 2 and at most maxGap wide.
func columnGap(ws []int, n, width int) int {
	if n <= 1 {
		return 0
	}
	lim := spreadTo
	if width > 0 {
		lim = min(width, spreadTo)
	}
	slack := max(lim-sumInts(ws)-2*(n-1), 0)
	return min(maxGap, 2+slack/(n-1))
}

// justifyRow pads r's cells to ws with gap between them, clipping each, then caps the line at width.
func justifyRow(r []string, n int, ws []int, gap, width int, clip func(string, int) string) string {
	var b strings.Builder
	for i, c := range r {
		start := sumInts(ws[:i]) + gap*i
		spans := spansRest(r, n, i)
		switch {
		case spans && width > 0 && width-start <= 0:
			c = ""
		case spans && width > 0:
			c = clip(c, width-start)
		case !spans:
			c = clip(c, ws[i])
		}
		b.WriteString(c)
		if i < len(r)-1 {
			b.WriteString(strings.Repeat(" ", ws[i]-utf8.RuneCountInString(c)+gap))
		}
	}
	return Truncate(strings.TrimRight(b.String(), " "), width)
}

// Render lays out a header plus rows as two-space-indented, column-aligned lines, no cell wider than its column.
// colMax caps each column's cells (0 or a missing entry = no cap).
func Render(head []string, colMax []int, rows [][]string, width int) string {
	all := make([][]string, 0, len(rows)+1)
	for _, r := range append([][]string{head}, rows...) {
		r = slices.Clone(r)
		for c := range r {
			if c < len(colMax) && colMax[c] > 0 {
				r[c] = Truncate(r[c], colMax[c])
			}
		}
		all = append(all, r)
	}
	return Indented(all, width, nil)
}

// Indented is Justify as text: every line two-space-indented, the indent counted in width.
func Indented(rows [][]string, width int, clip func(string, int) string) string {
	if width > 0 {
		width = max(width-2, 1)
	}
	var b strings.Builder
	for _, l := range Justify(rows, width, clip) {
		b.WriteString(strings.TrimRight("  "+l, " ") + "\n")
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
