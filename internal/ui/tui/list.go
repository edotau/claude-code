package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/table"
)

// ListRow is one selectable row of a List dialog.
type ListRow struct {
	Label, Detail, Status string // Status is the right-hand column, e.g. "environment (unset to disconnect)"
	OK                    bool   // draws a green ✓ before Status
	Section               string // rows sharing a Section group under "== <Section> ==", in first-seen order
}

// Bind is an extra key a List reacts to; Confirm non-empty makes it a double press on the same row.
type Bind struct {
	Key     Key
	Name    string // returned in ListResult.Action
	Help    string // footer text; empty keeps the bind out of the default footer
	Confirm string // prompt shown after the first press, e.g. "Press ctrl+d again to confirm"
}

// ListSpec describes a List dialog.
type ListSpec struct {
	Title  string
	Rows   []ListRow
	Search bool   // type-to-filter: printable keys build a query, Backspace edits it
	Binds  []Bind // extra key bindings
	Footer string // replaces the default hint line verbatim (binds are not appended to it)
}

// ListResult is the row chosen (index into ListSpec.Rows; -1 = back) and the bind that chose it ("" = Enter).
type ListResult struct {
	Index  int
	Action string
}

// listState is List's pure state: the transitions in listKey and the frame in listLines need no terminal.
type listState struct {
	spec    ListSpec
	order   []int // row indices grouped by section, first-seen order
	query   []rune
	view    []int // order filtered by query
	cur     int   // position in view
	pending int   // index into spec.Binds awaiting its second press, -1 none
	pendRow int   // original row index the pending press marked
}

func newListState(spec ListSpec, start int) listState {
	s := listState{spec: spec, pending: -1, pendRow: -1}
	var secs []string
	seen := map[string]bool{}
	for _, r := range spec.Rows {
		if !seen[r.Section] {
			seen[r.Section] = true
			secs = append(secs, r.Section)
		}
	}
	for _, sec := range secs {
		for i, r := range spec.Rows {
			if r.Section == sec {
				s.order = append(s.order, i)
			}
		}
	}
	s.view = s.order
	for p, i := range s.view {
		if i == start {
			s.cur = p
		}
	}
	return s
}

// selected is the original index under the cursor, -1 when the filter left nothing.
func (s listState) selected() int {
	if s.cur < 0 || s.cur >= len(s.view) {
		return -1
	}
	return s.view[s.cur]
}

// matches reports whether every whitespace-separated query term occurs (case-insensitively) in the row's text.
func (r ListRow) matches(query string) bool {
	hay := strings.ToLower(strings.Join([]string{r.Label, r.Detail, r.Status, r.Section}, "\x00"))
	for term := range strings.FieldsSeq(strings.ToLower(query)) {
		if !strings.Contains(hay, term) {
			return false
		}
	}
	return true
}

// refilter recomputes view, keeping the selected row when it survives and clamping the cursor otherwise.
func (s listState) refilter() listState {
	keep := s.selected()
	view := make([]int, 0, len(s.order))
	for _, i := range s.order {
		if s.spec.Rows[i].matches(string(s.query)) {
			view = append(view, i)
		}
	}
	s.view = view
	s.cur = min(s.cur, len(view)-1)
	for p, i := range view {
		if i == keep {
			s.cur = p
		}
	}
	if s.cur < 0 && len(view) > 0 {
		s.cur = 0
	}
	return s
}

// listKey is the pure transition: done reports a final result. Any key but the pending bind cancels a confirm.
func listKey(s listState, k Key) (listState, ListResult, bool) {
	pending, pendRow := s.pending, s.pendRow
	s.pending, s.pendRow = -1, -1
	search := s.spec.Search
	if s, res, done, handled := listNavKey(s, k, search); handled {
		return s, res, done
	}
	if s, res, done, handled := listBindKey(s, k, pending, pendRow); handled {
		return s, res, done
	}
	if search {
		s = listSearchKey(s, k)
	}
	return s, ListResult{}, false
}

// listNavKey handles cancel, cursor movement and enter; handled is false for any other key.
func listNavKey(s listState, k Key, search bool) (_ listState, res ListResult, done, handled bool) {
	vim := !search && k.Kind == KeyRune
	switch {
	case k.Kind == KeyCtrlC, k.Kind == KeyEsc, k.Kind == KeyLeft, vim && k.Rune == 'q':
		return s, ListResult{Index: -1}, true, true
	case k.Kind == KeyUp, vim && k.Rune == 'k':
		s.cur = max(0, s.cur-1)
		return s, ListResult{}, false, true
	case k.Kind == KeyDown, vim && k.Rune == 'j':
		s.cur = min(len(s.view)-1, s.cur+1)
		return s, ListResult{}, false, true
	case k.Kind == KeyEnter:
		if row := s.selected(); row >= 0 {
			return s, ListResult{Index: row}, true, true
		}
		return s, ListResult{}, false, true
	}
	return s, ListResult{}, false, false
}

// listBindKey fires the bind matching k on the selected row; a Confirm bind needs the same key twice on one row.
func listBindKey(s listState, k Key, pending, pendRow int) (_ listState, res ListResult, done, handled bool) {
	for bi, b := range s.spec.Binds {
		if b.Key != k {
			continue
		}
		row := s.selected()
		if row < 0 {
			return s, ListResult{}, false, true
		}
		if b.Confirm == "" || (pending == bi && pendRow == row) {
			return s, ListResult{Index: row, Action: b.Name}, true, true
		}
		s.pending, s.pendRow = bi, row
		return s, ListResult{}, false, true
	}
	return s, ListResult{}, false, false
}

// listSearchKey edits the search query and refilters.
func listSearchKey(s listState, k Key) listState {
	switch {
	case k.Kind == KeyBackspace && len(s.query) > 0:
		s.query = s.query[:len(s.query)-1]
		return s.refilter()
	case k.Kind == KeyRune && unicode.IsPrint(k.Rune):
		s.query = append(s.query, k.Rune)
		return s.refilter()
	}
	return s
}

// listFooter is the hint line: spec.Footer verbatim, or the default hints plus every bind that has Help.
func listFooter(spec ListSpec) string {
	if spec.Footer != "" {
		return spec.Footer
	}
	parts := []string{"↑/↓ select", "enter confirm"}
	if spec.Search {
		parts = append(parts, "type to search")
	}
	parts = append(parts, "esc back")
	for _, b := range spec.Binds {
		if b.Help != "" {
			parts = append(parts, b.Key.Name()+" "+b.Help)
		}
	}
	return strings.Join(parts, " · ")
}

// line is one frame line: plain (for wrap counting) and styled (what the terminal gets).
type line struct{ plain, styled string }

// oneLine folds newlines, tabs and carriage returns to a space so a row is one terminal row, as countLines assumes.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == '\t' }), " ")
}

func pad(s string, w int) string { return s + strings.Repeat(" ", max(0, w-textutil.RuneLen(s))) }

// listLines builds the frame: title, Search line, grouped rows (windowed to fit h-1 lines at cols), blank, footer.
func listLines(s listState, color bool, cols, h int) []line {
	head := listHead(s, color)
	lw, dw, hasSec := listColumns(s.spec.Rows)
	body, at := listBody(s, color, cols, lw, dw, hasSec)
	foot := listFootLine(s, color)
	tail := []line{{"", ""}, foot}
	body = window(body, max(at, 0), h-1-countLines(head, cols)-countLines(tail, cols), cols)
	return append(append(head, body...), tail...)
}

// listColumns sizes the label and detail columns over every row, so filtering never shifts them,
// and reports whether any row carries a section.
func listColumns(rows []ListRow) (lw, dw int, hasSec bool) {
	for _, r := range rows {
		lw, dw = max(lw, textutil.RuneLen(oneLine(r.Label))), max(dw, textutil.RuneLen(oneLine(r.Detail)))
		hasSec = hasSec || r.Section != ""
	}
	return lw, dw, hasSec
}

// listBody renders the visible rows with their section headings; at is the body index of the cursor row (-1 if none).
func listBody(s listState, color bool, cols, lw, dw int, hasSec bool) (body []line, at int) {
	at = -1
	sec := "\x00"
	for p, i := range s.view {
		r := s.spec.Rows[i]
		r.Label, r.Detail, r.Status = oneLine(r.Label), oneLine(r.Detail), oneLine(r.Status)
		if r.Section != sec {
			body = append(body, sectionBreak(color, hasSec, sec, r.Section)...)
			sec = r.Section
		}
		cells, styled := listRow(s, r, i, p, color, cols, lw, dw)
		if p == s.cur {
			at = len(body)
		}
		body = append(body, line{"  " + cells, styled})
	}
	if len(s.view) == 0 {
		body = append(body, line{"  no matches", "  " + ansi.Style(color, ansi.Dim, "no matches")})
	}
	return body, at
}

// sectionBreak is what precedes a row of section next when the previous row was in section prev ("\x00" = none).
func sectionBreak(color, hasSec bool, prev, next string) []line {
	var out []line
	if hasSec && next == "" && prev != "\x00" { // a section-less trailing row (Back) sits below a blank line
		out = append(out, line{})
	}
	if hasSec && next != "" {
		t := "== " + next + " =="
		out = append(out, line{}, line{t, Accent(color, t)}) // a blank line sets each section apart
	}
	return out
}

// listHead is the title and Search lines above the rows.
func listHead(s listState, color bool) []line {
	var head []line
	if s.spec.Title != "" {
		head = append(head, line{s.spec.Title, Accent(color, s.spec.Title)})
	}
	if s.spec.Search {
		q := string(s.query)
		head = append(head, line{"Search: " + q, ansi.Style(color, ansi.Dim, "Search: ") + q})
	}
	return head
}

// listFootLine is the footer hint, or the pending bind's confirm prompt.
func listFootLine(s listState, color bool) line {
	if s.pending >= 0 {
		p := s.spec.Binds[s.pending].Confirm
		return line{p, ansi.Style(color, ansi.Bold+ansi.Red, p)}
	}
	return line{listFooter(s.spec), ansi.Style(color, ansi.Dim, listFooter(s.spec))}
}

// listRow renders row r (spec index i, view position p) as its clipped cells and the styled line.
func listRow(s listState, r ListRow, i, p int, color bool, cols, lw, dw int) (cells, styled string) {
	if cols > 0 { // clip the status to the row's room (2 = marker prefix, 2 = gap, 2 = "✓ ") so a row never wraps
		room := cols - 2 - lw - 2 - dw - 2
		if r.OK {
			room -= 2
		}
		r.Status = table.Truncate(r.Status, max(room, 0))
	}
	status := r.Status
	if r.OK {
		status = strings.TrimSpace("✓ " + status)
	}
	full := strings.TrimRight(pad(r.Label, lw)+"  "+pad(r.Detail, dw)+"  "+status, " ")
	cells = table.Truncate(full, cols-2) // width <= 0 is unbounded
	switch {
	case i == s.pendRow:
		styled = ansi.Style(color, ansi.Red, "▸") + " " + ansi.Style(color, ansi.Red+"\x1b[7m", cells)
	case p == s.cur:
		styled = AccentMark(color, "▸") + " " + Selected(color, cells)
	case cells != full: // clipped: print exactly what countLines measured, or the wrap leaves stale frames
		styled = "  " + cells
	default:
		styled = plainRowStyled(color, r, lw, dw)
	}
	return cells, styled
}

// plainRowStyled is the unselected, unclipped row: padded label, muted detail, then the styled status.
func plainRowStyled(color bool, r ListRow, lw, dw int) string {
	st := ansi.Status(color, r.Status)
	if r.OK {
		st = strings.TrimSpace(ansi.Style(color, ansi.Green, "✓") + " " + st)
	}
	detail := r.Detail // pad only when a status follows: padding inside the dim escape survives TrimRight
	if st != "" {
		detail = pad(r.Detail, dw)
	}
	styled := "  " + pad(r.Label, lw)
	if detail != "" || st != "" {
		styled += "  " + Muted(color, detail)
	}
	if st != "" {
		styled += "  " + st
	}
	return styled
}

// countLines is how many terminal rows ls occupy at cols columns once long lines wrap.
func countLines(ls []line, cols int) int {
	n := 0
	for _, l := range ls {
		n += wrappedLines(textutil.RuneLen(l.plain), cols)
	}
	return n
}

// grow widens [at, at+1) over n items, hi then lo each pass, while fits(lo, hi) holds.
func grow(n, at int, fits func(lo, hi int) bool) (lo, hi int) {
	lo, hi = at, at+1
	for grew := true; grew; {
		grew = false
		if hi < n && fits(lo, hi+1) {
			hi, grew = hi+1, true
		}
		if lo > 0 && fits(lo-1, hi) {
			lo, grew = lo-1, true
		}
	}
	return lo, hi
}

// window keeps the slice of ls around index at that fits in room terminal rows (at least the cursor line).
func window(ls []line, at, room, cols int) []line {
	if countLines(ls, cols) <= room || at >= len(ls) {
		return ls
	}
	lo, hi := grow(len(ls), at, func(lo, hi int) bool { return countLines(ls[lo:hi], cols) <= room })
	return ls[lo:hi]
}

// renderList joins a frame the way render does: every line cleared to its end, no trailing newline.
func renderList(ls []line) string {
	styled := make([]string, len(ls))
	for i, l := range ls {
		styled[i] = l.styled + "\x1b[K"
	}
	return strings.Join(styled, "\r\n")
}

// List runs a grouped, optionally searchable single-choice dialog opening on row start. A non-nil error means stdin
// is not a terminal (Index -1), the same contract as Pick.
func List(spec ListSpec, start int) (res ListResult, err error) {
	t, err := Open()
	if err != nil {
		return ListResult{Index: -1}, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	s := newListState(spec, start)
	color := ansi.Enabled(os.Stdout)
	prev := 0 // terminal rows the last frame drew: the next redraw rewinds exactly that many
	rewind := func() {
		if prev > 1 {
			fmt.Fprintf(os.Stdout, "\r\x1b[%dA", prev-1)
		}
		fmt.Fprint(os.Stdout, "\r\x1b[J")
	}
	draw := func() {
		cols, h := t.Size()
		ls := listLines(s, color, cols, h)
		if prev > 0 {
			rewind()
		}
		fmt.Fprint(os.Stdout, renderList(ls))
		prev = countLines(ls, cols)
	}
	draw()
	defer rewind()
	for k := range t.Keys() {
		var done bool
		if s, res, done = listKey(s, k); done {
			return res, nil
		}
		draw()
	}
	return ListResult{Index: -1}, t.Err()
}
