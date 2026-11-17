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

// Justify lays rows out as plain column-aligned lines within width (<= 0 = uncapped); a short last cell spans the rest.
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

const spaces64 = "                                                                "

func writeSpaces(b *strings.Builder, n int) {
	for n >= len(spaces64) {
		b.WriteString(spaces64)
		n -= len(spaces64)
	}
	if n > 0 {
		b.WriteString(spaces64[:n])
	}
}

// justifyRow pads r's cells to ws with gap between them, clipping each, then caps the line at width.
func justifyRow(r []string, n int, ws []int, gap, width int, clip func(string, int) string) string {
	var b strings.Builder
	start := 0
	for i, c := range r {
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
			writeSpaces(&b, ws[i]-utf8.RuneCountInString(c)+gap)
		}
		start += ws[i] + gap
	}
	return Truncate(strings.TrimRight(b.String(), " "), width)
}

// Render lays out a header plus rows as two-space-indented aligned lines; colMax caps each column (0 = none).
func Render(head []string, colMax []int, rows [][]string, width int) string {
	all := make([][]string, len(rows)+1)
	all[0] = capRow(head, colMax)
	for i, r := range rows {
		all[i+1] = capRow(r, colMax)
	}
	return Indented(all, width, nil)
}

func capRow(r []string, colMax []int) []string {
	var cloned []string
	for c, cell := range r {
		if c >= len(colMax) || colMax[c] <= 0 {
			continue
		}
		capped := Truncate(cell, colMax[c])
		if capped != cell {
			if cloned == nil {
				cloned = slices.Clone(r)
			}
			cloned[c] = capped
		}
	}
	if cloned != nil {
		return cloned
	}
	return r
}

// Indented is Justify as text: every line two-space-indented, the indent counted in width.
func Indented(rows [][]string, width int, clip func(string, int) string) string {
	if width > 0 {
		width = max(width-2, 1)
	}
	lines := Justify(rows, width, clip)
	var b strings.Builder
	b.Grow(len(lines) * 48)
	for _, l := range lines {
		if trimmed := strings.TrimRight(l, " "); trimmed != "" {
			b.WriteString("  ")
			b.WriteString(trimmed)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Styled is Render with a style code per row (painted after alignment, so padding sees plain text).
func Styled(head []string, colMax []int, rows [][]string, width int, codes []string, color bool) string {
	lines := strings.Split(strings.TrimRight(Render(head, colMax, rows, width), "\n"), "\n")
	for i, l := range lines {
		code := ansi.Dim
		if i > 0 && i <= len(codes) {
			code = codes[i-1]
		}
		lines[i] = ansi.Style(color && code != "", code, l)
	}
	return strings.Join(lines, "\n") + "\n"
}

// DropEmptyCols removes every column past the first that is empty in all rows, trimming head and colMax with it.
func DropEmptyCols(head []string, colMax []int, rows [][]string) ([]string, []int, [][]string) {
	cell := func(r []string, c int) string {
		if c < len(r) {
			return r[c]
		}
		return ""
	}
	var keep []int
	for c := range head {
		if c == 0 || slices.ContainsFunc(rows, func(r []string) bool { return cell(r, c) != "" }) {
			keep = append(keep, c)
		}
	}
	pick := func(r []string) []string {
		out := make([]string, len(keep))
		for i, c := range keep {
			out[i] = cell(r, c)
		}
		return out
	}
	cols := make([]int, len(keep))
	outRows := make([][]string, len(rows))
	for i, c := range keep {
		if c < len(colMax) {
			cols[i] = colMax[c]
		}
	}
	for i, r := range rows {
		outRows[i] = pick(r)
	}
	return pick(head), cols, outRows
}
