// view.go is the dashboard's pure state: which nodes are open, the selection, the scroll, and the visible rows.

package tree

import (
	"cmp"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/cli/ui/ansi"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// dashFooter is the key hint under the dashboard's rows.
const dashFooter = "↑↓ move · ←→ fold · enter expand · o open · c cancel · y id · r result · q quit"

// footerRows are the lines under the rows: the status line (blank when unset) and dashFooter.
const footerRows = 2

const (
	headerHeight = 2
	minTreeRows  = 3
)

// maxLineWidth caps a dashboard line on a wide terminal: CLI and TUI rows stay within 80 columns.
const maxLineWidth = 80

// dashState is the dashboard between frames; every transition returns a new state.
type dashState struct {
	open          map[string]bool // explicit expand state by nodeKey; an unset root is open, anything else closed
	expanded      map[string]bool // nodeKeys of the dispatch and subagent rows whose detail lines show
	sel           int             // index of the selection in the visible rows
	selID         string          // nodeKey of the selection, kept across reloads
	top           int             // first visible row in the viewport
	count         int             // visible rows at the last clamp
	width, height int
	pending       string // job id an unconfirmed cancel names
	status        string // the last action's line, shown above the footer
	header        *headerStatus
}

// expandKey names a row's expansion: a dispatch by its own id, which a carrier keeps when its worker takes over.
func expandKey(n Node) string {
	if n.Kind == KindDispatch && n.Dispatch != nil {
		return string(n.Kind) + ":" + n.Dispatch.ID
	}
	return nodeKey(n)
}

// nodeKey names a node across reloads; kinds keep a session and a dispatch with one id apart.
func nodeKey(n Node) string { return string(n.Kind) + ":" + n.ID }

func isOpen(open map[string]bool, n Node) bool {
	if v, ok := open[nodeKey(n)]; ok {
		return v
	}
	return n.Depth == 0
}

// visible is nodes flattened in print order, the children of closed nodes left out.
func visible(nodes []Node, open map[string]bool) []Node {
	var out []Node
	for _, n := range nodes {
		out = append(out, n)
		if isOpen(open, n) {
			out = append(out, visible(n.Children, open)...)
		}
	}
	return out
}

func headerRows(s dashState) int {
	if s.header != nil && s.height >= footerRows+headerHeight+minTreeRows {
		return headerHeight
	}
	return 0
}

// viewRows reserves header space only when three tree rows still fit.
func viewRows(s dashState) int { return max(s.height-footerRows-headerRows(s), 1) }

// clamp re-finds the selection in vis by id (else keeps its index), then scrolls it into the viewport.
func clamp(s dashState, vis []Node) dashState {
	s.count = len(vis)
	if len(vis) == 0 {
		s.sel, s.selID, s.top = 0, "", 0
		return s
	}
	if i := slices.IndexFunc(vis, func(n Node) bool { return nodeKey(n) == s.selID }); i >= 0 {
		s.sel = i
	}
	s.sel = min(max(s.sel, 0), len(vis)-1)
	s.selID = nodeKey(vis[s.sel])
	return clampTop(s)
}

// clampTop keeps the selection inside the viewport and the viewport inside the rows.
func clampTop(s dashState) dashState {
	rows := viewRows(s)
	s.top = min(s.top, s.sel)
	if s.sel >= s.top+rows {
		s.top = s.sel - rows + 1
	}
	s.top = max(min(s.top, s.count-rows), 0)
	return s
}

// onResize is s laid out for a w x h terminal.
func onResize(s dashState, w, h int) dashState {
	s.width, s.height = w, h
	return clampTop(s)
}

// keyActions are the keys that act on the selected job.
var keyActions = map[key]action{keyCancel: actCancel, keyCopy: actCopy, keyResult: actResult}

// onKey answers an armed cancel with any key, else runs an action key, else moves or opens or closes the selection.
func onKey(s dashState, k key, nodes []Node) (dashState, effect) {
	vis := visible(nodes, s.open)
	if s = clamp(s, vis); len(vis) == 0 {
		return s, effect{}
	}
	var e effect
	if s.pending != "" {
		s.pending, e = onConfirm(s.pending, k == keyConfirm, vis[s.sel])
		return s, e
	}
	if a, ok := keyActions[k]; ok {
		s.pending, e = onAction(s.pending, a, vis[s.sel])
		return s, e
	}
	if k == keyEnter && detailed(vis[s.sel]) {
		key := expandKey(vis[s.sel])
		s.expanded = withOpen(s.expanded, key, !s.expanded[key])
		return s, e
	}
	s = move(s, k, vis)
	return clamp(s, visible(nodes, s.open)), e
}

// detailed reports whether n can show detail lines under its row.
func detailed(n Node) bool {
	return n.Kind == KindDispatch && n.Dispatch != nil || n.Kind == KindSubagent && n.Subagent != nil
}

// expansion is the lines under an expanded row: the task or description, then the identity line.
func expansion(n Node) []string {
	if n.Kind == KindSubagent {
		s := *n.Subagent
		return []string{"└─ " + textutil.OneLine(s.Description),
			"   " + cmp.Or(s.Type, "subagent") + " · " + strings.TrimPrefix(subModel(s), databricksPrefix)}
	}
	return dispatchExpansion(*n.Dispatch)
}

// move applies k to the selection s.sel of the non-empty vis; right on an open node steps into it, left to the parent.
func move(s dashState, k key, vis []Node) dashState {
	ids := nodeKeys(vis)
	cur := vis[s.sel]
	opened := len(cur.Children) > 0 && isOpen(s.open, cur)
	switch {
	case k == keyUp:
		s.selID = pick(ids, s.selID, -1)
	case k == keyDown, k == keyRight && opened:
		s.selID = pick(ids, s.selID, 1)
	case k == keyRight && len(cur.Children) > 0:
		s.open = withOpen(s.open, s.selID, true)
	case k == keyLeft && opened:
		s.open = withOpen(s.open, s.selID, false)
	case k == keyLeft:
		s.selID = ids[parentRow(vis, s.sel)]
	}
	return s
}

func nodeKeys(vis []Node) []string {
	ids := make([]string, len(vis))
	for i, n := range vis {
		ids[i] = nodeKey(n)
	}
	return ids
}

// pick moves the selection by step over ids, clamped; a vanished or empty selection lands on the first row.
func pick(ids []string, sel string, step int) string {
	if len(ids) == 0 {
		return ""
	}
	i := slices.Index(ids, sel)
	if i < 0 {
		return ids[0]
	}
	return ids[min(max(i+step, 0), len(ids)-1)]
}

// withOpen is a copy of open with id set to v.
func withOpen(open map[string]bool, id string, v bool) map[string]bool {
	out := maps.Clone(open)
	if out == nil {
		out = map[string]bool{}
	}
	out[id] = v
	return out
}

// parentRow is the index of row i's parent in vis, i itself for a root.
func parentRow(vis []Node, i int) int {
	for j := i - 1; j >= 0; j-- {
		if vis[j].Depth < vis[i].Depth {
			return j
		}
	}
	return i
}

// rowCtx is what a row needs from the rows above it: the owning session's cwd, and whether a session owns it.
type rowCtx struct {
	now       time.Time
	home, cwd string
	open      map[string]bool
	inSession bool
	detail    bool // printed --detail: expansion lines under each dispatch row
	sub       Node // the nearest subagent row above, whose dispatches nest under it
}

// dashLines is the frame: the viewport's rows, laid out by the tree's one row formatter, then the footer.
func dashLines(s dashState, nodes []Node, now time.Time, paint string) []string {
	vis := visible(nodes, s.open)
	width := min(max(s.width, 1), maxLineWidth)
	var head []string
	if headerRows(s) != 0 {
		head = headerLines(*s.header, width)
	}
	if len(vis) == 0 {
		return append(head, "agents dashboard: no sessions in scope", s.status, dashFooter)
	}
	home, _ := os.UserHomeDir()
	c := rowCtx{now: now, home: home, open: s.open}
	l := layout{active: true}
	starts := make([]int, len(vis)+1)
	for i, n := range vis {
		if n.Depth == 0 {
			c.cwd, c.inSession = "", n.Kind == KindSession
		}
		starts[i] = len(l.rows)
		addNode(&l, n, &c, i == s.sel)
		if s.expanded[expandKey(n)] && detailed(n) {
			indent := l.rows[len(l.rows)-1].indent + textutil.DisplayWidth(l.rows[len(l.rows)-1].cells[0]) + len(gap)
			for _, line := range expansion(n) {
				l.free(indent, line)
			}
		}
	}
	starts[len(vis)] = len(l.rows)
	lines := l.lines(width, paint)
	top := min(starts[min(s.top, len(vis))], len(lines))
	if end := starts[min(s.sel+1, len(vis))]; end > top+viewRows(s) {
		top = min(end-viewRows(s), starts[s.sel]) // the selection's detail lines show when they fit, its row always
	}
	lines = lines[top:min(top+viewRows(s), len(lines))]
	foot := ansi.Style(paint != "", ansi.Dim, textutil.TruncateCells(dashFooter, width, "…"))
	return append(append(head, lines...), textutil.TruncateCells(s.status, width, "…"), foot)
}

// addNode adds n's own row: a ▸/▾ twist, the depth's indent, then the cells `agents tree` prints for its kind.
func addNode(l *layout, n Node, c *rowCtx, sel bool) {
	twist := "  "
	if len(n.Children) > 0 {
		twist = map[bool]string{true: "▾ ", false: "▸ "}[isOpen(c.open, n)]
	}
	switch n.Kind {
	case KindSession:
		c.cwd = n.Session.Cwd
		addSessionRow(l, *n.Session, c.now, twist)
	case KindWorkflow:
		addRunRow(l, *n.Workflow, c.now, twist)
	case KindSubagent:
		c.sub = n
		addSubagentRow(l, *n.Subagent, c, twist, n.Subagent.running())
	default:
		level, marker := levelJob, twist+strings.Repeat("  ", n.Depth)+jobMarker(*n.Dispatch)
		if c.inSession { // the printed tree's ↳ row; a guide column per level below the subagent
			guide := strings.Repeat("   ", max(n.Depth-c.sub.Depth-1, 0))
			level, marker = levelDispatch, twist+subNest(*c.sub.Subagent)+guide+"↳"
		}
		addJobRow(l, level, append([]string{marker}, jobCells(*n.Dispatch, c.now, nil)...), *n.Dispatch, sel)
		return
	}
	l.spanLast(false, sel)
}
