package tui

import (
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ansi"
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
	for _, term := range strings.Fields(strings.ToLower(query)) {
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
	back := ListResult{Index: -1}
	search := s.spec.Search
	switch {
	case k.Kind == KeyCtrlC, k.Kind == KeyEsc, k.Kind == KeyLeft, !search && k.Kind == KeyRune && k.Rune == 'q':
		return s, back, true
	case k.Kind == KeyUp, !search && k.Kind == KeyRune && k.Rune == 'k':
		s.cur = max(0, s.cur-1)
		return s, ListResult{}, false
	case k.Kind == KeyDown, !search && k.Kind == KeyRune && k.Rune == 'j':
		s.cur = min(len(s.view)-1, s.cur+1)
		return s, ListResult{}, false
	case k.Kind == KeyEnter:
		if row := s.selected(); row >= 0 {
			return s, ListResult{Index: row}, true
		}
		return s, ListResult{}, false
	}
	for bi, b := range s.spec.Binds {
		if b.Key != k {
			continue
		}
		row := s.selected()
		if row < 0 {
			return s, ListResult{}, false
		}
		if b.Confirm == "" || (pending == bi && pendRow == row) {
			return s, ListResult{Index: row, Action: b.Name}, true
		}
		s.pending, s.pendRow = bi, row
		return s, ListResult{}, false
	}
	if search {
		switch {
		case k.Kind == KeyBackspace && len(s.query) > 0:
			s.query = s.query[:len(s.query)-1]
			s = s.refilter()
		case k.Kind == KeyRune && unicode.IsPrint(k.Rune):
			s.query = append(s.query, k.Rune)
			s = s.refilter()
		}
	}
	return s, ListResult{}, false
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

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func pad(s string, w int) string { return s + strings.Repeat(" ", max(0, w-runeLen(s))) }

// listLines builds the frame: title, Search line, grouped rows (windowed to fit h-1 lines at cols), blank, footer.
func listLines(s listState, color bool, cols, h int) []line {
	var head, body []line
	if s.spec.Title != "" {
		head = append(head, line{s.spec.Title, ansi.Style(color, ansi.Bold+ansi.Cyan, s.spec.Title)})
	}
	if s.spec.Search {
		q := string(s.query)
		head = append(head, line{"Search: " + q, ansi.Style(color, ansi.Dim, "Search: ") + q})
	}
	lw, dw := 0, 0 // column widths over every row, so filtering never shifts the columns
	hasSec := false
	for _, r := range s.spec.Rows {
		lw, dw = max(lw, runeLen(r.Label)), max(dw, runeLen(r.Detail))
		hasSec = hasSec || r.Section != ""
	}
	at := -1 // body index of the cursor row
	sec := "\x00"
	for p, i := range s.view {
		r := s.spec.Rows[i]
		if r.Section != sec {
			sec = r.Section
			if hasSec && sec != "" {
				t := "== " + sec + " =="
				body = append(body, line{t, ansi.Style(color, ansi.Bold+ansi.Cyan, t)})
			}
		}
		status := r.Status
		if r.OK {
			status = strings.TrimSpace("✓ " + status)
		}
		cells := strings.TrimRight(pad(r.Label, lw)+"  "+pad(r.Detail, dw)+"  "+status, " ")
		var styled string
		switch {
		case i == s.pendRow:
			styled = ansi.Style(color, ansi.Red, "▸") + " " + ansi.Style(color, ansi.Red+"\x1b[7m", cells)
		case p == s.cur:
			styled = ansi.Style(color, ansi.Cyan, "▸") + " " + ansi.Style(color, "\x1b[7m", cells)
		default:
			st := ansi.Status(color, r.Status)
			if r.OK {
				st = strings.TrimSpace(ansi.Style(color, ansi.Green, "✓") + " " + st)
			}
			styled = "  " + strings.TrimRight(pad(r.Label, lw)+"  "+ansi.Style(color, ansi.Dim, pad(r.Detail, dw))+"  "+st, " ")
		}
		if p == s.cur {
			at = len(body)
		}
		body = append(body, line{"  " + cells, styled})
	}
	if len(s.view) == 0 {
		body = append(body, line{"  no matches", "  " + ansi.Style(color, ansi.Dim, "no matches")})
	}
	foot := line{listFooter(s.spec), ansi.Style(color, ansi.Dim, listFooter(s.spec))}
	if s.pending >= 0 {
		p := s.spec.Binds[s.pending].Confirm
		foot = line{p, ansi.Style(color, ansi.Bold+ansi.Red, p)}
	}
	tail := []line{{"", ""}, foot}
	body = window(body, max(at, 0), h-1-countLines(head, cols)-countLines(tail, cols), cols)
	return append(append(head, body...), tail...)
}

// countLines is how many terminal rows ls occupy at cols columns once long lines wrap.
func countLines(ls []line, cols int) int {
	n := 0
	for _, l := range ls {
		n += wrappedLines(runeLen(l.plain), cols)
	}
	return n
}

// window keeps the slice of ls around index at that fits in room terminal rows (at least the cursor line).
func window(ls []line, at, room, cols int) []line {
	if countLines(ls, cols) <= room || at >= len(ls) {
		return ls
	}
	lo, hi := at, at+1
	for grew := true; grew; {
		grew = false
		if hi < len(ls) && countLines(ls[lo:hi+1], cols) <= room {
			hi, grew = hi+1, true
		}
		if lo > 0 && countLines(ls[lo-1:hi], cols) <= room {
			lo, grew = lo-1, true
		}
	}
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
func List(spec ListSpec, start int) (ListResult, error) {
	t, err := Open()
	if err != nil {
		return ListResult{Index: -1}, err
	}
	defer t.Close()
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
		var res ListResult
		var done bool
		if s, res, done = listKey(s, k); done {
			return res, nil
		}
		draw()
	}
	return ListResult{Index: -1}, nil
}
