// columns.go lays the tree's rows out so each level's fields start in the same column on every row.

package tree

import (
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// Row levels: each level pads its columns to the widest cell among the rows of that level in one render.
const (
	levelFree     = iota // a single text cell: repo headers, phase lines, blank separators
	levelSession         // label state age branch | title
	levelRun             // "workflow <name>" state age | id
	levelSubagent        // marker type model ↑in ↓out total age idle | description
	levelDispatch        // marker agent state duration signal | model
	levelJob             // id where model duration signal | task
	levelCount
)

// col shapes one fixed column; the last cell of every row is free text, clipped to the width and never padded.
type col struct {
	right    bool // right-aligned: numbers and durations
	max      int  // widest the column may grow on a terminal; 0 = unlimited
	optional bool // dropped for the whole level when keeping it would leave the text under minText
	fit      bool // a model id: whole unless the text would fall under minFitText, then clipped with an ellipsis
}

// levelCols holds the fixed columns per level; the cells of a row are these columns, then the text.
var levelCols = [levelCount][]col{
	levelSession:  {{max: 20}, {}, {right: true}, {max: 12}, {right: true}},
	levelRun:      {{}, {}, {right: true}},
	levelSubagent: {{}, {max: 10}, {fit: true}, {right: true}, {right: true}, {right: true}, {right: true, optional: true}, {right: true, optional: true}},
	levelDispatch: {{}, {max: 14}, {}, {right: true}, {}},
	levelJob:      {{}, {max: 15}, {fit: true}, {right: true}, {}},
}

// levelIndent is the left indent of each level's rows.
var levelIndent = [levelCount]int{levelSession: 2, levelRun: 4, levelSubagent: 4, levelDispatch: 6, levelJob: 2}

// minText is the room the text must keep on a terminal before an optional column is dropped.
const minText = 24

// minFitText is the text a fit column leaves, and minFitWidth the narrowest it is clipped to.
const (
	minFitText  = 8
	minFitWidth = 8
)

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

// lines builds one line per row, clipped to width (0 = whole); it clips the rows' cells in place, so call it once.
func (l *layout) lines(width int, paint string) []string {
	rows := l.capped(width)
	widths := levelWidths(rows)
	dropOptional(widths, width)
	fitModels(rows, widths, width)
	out := make([]string, len(rows))
	for i, r := range rows {
		ws := widths[r.level]
		var leafWidths [8]int
		if r.level == levelSubagent && r.cells[5] == "" && width > 0 &&
			width-prefixWidth(ws, r.indent) < minText {
			copy(leafWidths[:], ws)
			leafWidths[5] = 0
			ws = leafWidths[:len(ws)]
		}
		out[i] = r.line(ws, width, paint)
	}
	return out
}

// capped returns rows whose fixed cells are clipped to their column's max, for a terminal only.
func (l *layout) capped(width int) []row {
	out := l.rows
	for i, r := range out {
		for c, k := range levelCols[r.level] {
			if width > 0 && k.max > 0 {
				out[i].cells[c] = textutil.TruncateCells(r.cells[c], k.max, "…")
			}
		}
	}
	return out
}

// levelWidths is each level's widest cell per fixed column, in display cells.
func levelWidths(rows []row) [levelCount][]int {
	var ws [levelCount][]int
	for level, cols := range levelCols {
		ws[level] = make([]int, len(cols))
	}
	for _, r := range rows {
		for c := range levelCols[r.level] {
			ws[r.level][c] = max(ws[r.level][c], textutil.DisplayWidth(r.cells[c]))
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

// fitModels clips a level's fit column, cells and width, so its text keeps minFitText columns when it can.
func fitModels(rows []row, ws [levelCount][]int, width int) {
	if width <= 0 {
		return
	}
	for level, cols := range levelCols {
		for c, k := range cols {
			over := minFitText - (width - prefixWidth(ws[level], levelIndent[level]))
			if !k.fit || ws[level][c] == 0 || over <= 0 {
				continue
			}
			ws[level][c] = max(ws[level][c]-over, min(minFitWidth, ws[level][c]))
			for i, r := range rows {
				if r.level == level {
					rows[i].cells[c] = textutil.TruncateCells(r.cells[c], ws[level][c], "…")
				}
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
func (r row) line(ws []int, width int, paint string) string {
	var b strings.Builder
	extra := 0
	if r.hot != 0 {
		extra = len(levelCols[r.level]) * (len(paint) + len(ansi.Reset))
	}
	b.Grow(max(width, prefixWidth(ws, r.indent)+len(r.cells[len(r.cells)-1])) + extra)
	b.WriteString(strings.Repeat(" ", r.indent))
	pos := r.indent
	for c := range levelCols[r.level] {
		if ws[c] == 0 {
			continue
		}
		r.writeCell(&b, c, ws[c], width <= 0 || pos+ws[c] <= width, paint)
		pos += ws[c] + len(gap)
		b.WriteString(gap)
	}
	text := r.cells[len(r.cells)-1]
	if avail := width - prefixWidth(ws, r.indent); width > 0 {
		if avail <= 0 {
			text = ""
		} else {
			text = textutil.TruncateCells(text, avail, "…")
		}
	}
	b.WriteString(text)
	line := strings.TrimRight(b.String(), " ")
	if width > 0 && prefixWidth(ws, r.indent) > width {
		line = textutil.TruncateCells(line, width, "…")
	}
	return line
}

func (r row) writeCell(b *strings.Builder, c, size int, visible bool, paint string) {
	hot := paint != "" && r.hot>>c&1 != 0 && visible
	if hot {
		b.WriteString(paint)
	}
	b.WriteString(textutil.PadCells(r.cells[c], size, levelCols[r.level][c].right))
	if hot {
		b.WriteString(ansi.Reset)
	}
}
