// columns.go lays the tree's rows out so each level's fields start in the same column on every row.

package tree

import (
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// Row levels: each level pads its columns to the widest cell among the rows of that level in one render.
const (
	levelFree     = iota // a single text cell: repo headers, phase lines, blank separators
	levelSession         // label state age branch | title
	levelRun             // "workflow <name>" state age | id
	levelSubagent        // marker type state ↑in ↓out age idle tool | description
	levelDispatch        // marker agent state duration signal | model
	levelJob             // id where model duration signal | task
	levelCount
)

// col shapes one fixed column; the last cell of every row is free text, clipped to the width and never padded.
type col struct {
	right    bool // right-aligned: numbers and durations
	max      int  // widest the column may grow on a terminal; 0 = unlimited
	optional bool // dropped for the whole level when keeping it would leave the text under minText
}

// levelCols holds the fixed columns per level; the cells of a row are these columns, then the text.
var levelCols = [levelCount][]col{
	levelSession:  {{max: 20}, {}, {right: true}, {max: 12}},
	levelRun:      {{}, {}, {right: true}},
	levelSubagent: {{}, {max: 14}, {}, {right: true}, {right: true}, {right: true}, {right: true}, {max: 14, optional: true}},
	levelDispatch: {{}, {max: 14}, {}, {right: true}, {}},
	levelJob:      {{}, {max: 15}, {max: 22}, {right: true}, {}},
}

// levelIndent is the left indent of each level's rows.
var levelIndent = [levelCount]int{levelSession: 2, levelRun: 4, levelSubagent: 4, levelDispatch: 6, levelJob: 2}

// minText is the room the text must keep on a terminal before an optional column is dropped.
const minText = 24

// gap separates the columns of a row, and the last column from the text.
const gap = "  "

type row struct {
	level, indent int
	cells         []string
	hot           uint8 // bit c set: fixed cell c is painted when the layout is rendered with a paint
}

// layout collects rows in print order so column widths can be measured across a whole level before any line is built.
type layout struct {
	rows   []row
	active bool // rows added now belong to the active section, the only one mark paints
}

func (l *layout) add(level int, cells ...string) {
	l.rows = append(l.rows, row{level: level, indent: levelIndent[level], cells: cells})
}

// mark makes the cells in bits of the last row hot when it sits in the active section and on is true.
func (l *layout) mark(on bool, bits uint8) {
	if on && l.active {
		l.rows[len(l.rows)-1].hot = bits
	}
}

// sep adds a blank row unless the layout is empty or already ends in one.
func (l *layout) sep() {
	if n := len(l.rows); n > 0 && (l.rows[n-1].level != levelFree || l.rows[n-1].cells[0] != "") {
		l.free(0, "")
	}
}

func (l *layout) free(indent int, text string) {
	l.rows = append(l.rows, row{level: levelFree, indent: indent, cells: []string{text}})
}

// lines builds one line per row, clipped to width (0 = whole); paint wraps hot cells after, so escapes add no width.
func (l *layout) lines(width int, paint string) []string {
	rows := l.capped(width)
	widths := levelWidths(rows)
	dropOptional(widths, width)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.line(widths[r.level], width)
		if paint != "" && r.hot != 0 {
			out[i] = r.paintHot(out[i], widths[r.level], paint)
		}
	}
	return out
}

// capped returns rows whose fixed cells are clipped to their column's max, for a terminal only.
func (l *layout) capped(width int) []row {
	out := make([]row, len(l.rows))
	for i, r := range l.rows {
		r.cells = append([]string(nil), r.cells...)
		for c, k := range levelCols[r.level] {
			if width > 0 && k.max > 0 {
				r.cells[c] = textutil.Truncate(r.cells[c], k.max)
			}
		}
		out[i] = r
	}
	return out
}

// levelWidths is each level's widest cell per fixed column, in runes (the tree's glyphs are one column each).
func levelWidths(rows []row) [levelCount][]int {
	var ws [levelCount][]int
	for level, cols := range levelCols {
		ws[level] = make([]int, len(cols))
	}
	for _, r := range rows {
		for c := range levelCols[r.level] {
			ws[r.level][c] = max(ws[r.level][c], textutil.RuneLen(r.cells[c]))
		}
	}
	return ws
}

// dropOptional zeroes an optional column's width, which hides it, when the level's text would get under minText.
func dropOptional(ws [levelCount][]int, width int) {
	if width <= 0 {
		return
	}
	for level, cols := range levelCols {
		for c, k := range cols {
			if k.optional && ws[level][c] > 0 && width-prefixWidth(ws[level], levelIndent[level]) < minText {
				ws[level][c] = 0
			}
		}
	}
}

// prefixWidth is the columns the indent and the visible fixed columns take, each followed by a gap.
func prefixWidth(ws []int, indent int) int {
	n := indent
	for _, w := range ws {
		if w > 0 {
			n += w + len(gap)
		}
	}
	return n
}

// line pads r's visible fixed columns to ws, then the text, and clips the whole row to width (0 = no clip).
func (r row) line(ws []int, width int) string {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", r.indent))
	for c, k := range levelCols[r.level] {
		if ws[c] == 0 {
			continue
		}
		cell, pad := r.cells[c], strings.Repeat(" ", ws[c]-textutil.RuneLen(r.cells[c]))
		if k.right {
			b.WriteString(pad + cell + gap)
		} else {
			b.WriteString(cell + pad + gap)
		}
	}
	text := r.cells[len(r.cells)-1]
	if avail := width - textutil.RuneLen(b.String()); width > 0 && avail > 0 {
		text = textutil.Truncate(text, avail)
	}
	line := strings.TrimRight(b.String()+text, " ")
	if width > 0 {
		line = textutil.Truncate(line, width)
	}
	return line
}

// paintHot wraps r's hot cells found intact in the clipped line with paint and a reset.
func (r row) paintHot(line string, ws []int, paint string) string {
	var b strings.Builder
	b.Grow(len(line) + 2*(len(paint)+len(ansi.Reset)))
	col, runes, at, written := r.indent, 0, 0, 0 // col: next cell's column; runes/at: how far line has been walked
	for c, k := range levelCols[r.level] {
		if ws[c] == 0 {
			continue
		}
		cell, start := r.cells[c], col
		if k.right {
			start += ws[c] - textutil.RuneLen(cell)
		}
		col += ws[c] + len(gap)
		if r.hot>>c&1 == 0 || cell == "" {
			continue
		}
		for ; runes < start && at < len(line); runes++ {
			_, n := utf8.DecodeRuneInString(line[at:])
			at += n
		}
		if runes != start || !strings.HasPrefix(line[at:], cell) {
			continue
		}
		b.WriteString(line[written:at])
		b.WriteString(paint)
		b.WriteString(cell)
		b.WriteString(ansi.Reset)
		written = at + len(cell)
	}
	b.WriteString(line[written:])
	return b.String()
}
