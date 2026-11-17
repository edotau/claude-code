// render.go renders the tree data for the terminal.

package tree

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// maxRowWidth is the widest a terminal row may be, indent included; wider rows clip with an ellipsis.
const maxRowWidth = 80

// rowWidth is the clip width for w: its terminal columns capped at maxRowWidth, 0 when piped (rows stay whole).
func rowWidth(w io.Writer) int {
	n := helptext.TermWidth(w)
	if n <= 0 {
		return 0
	}
	return min(n, maxRowWidth)
}

// activePaint is the SGR prefix for active rows: the "minimal" theme's first ramp stop, "" when w takes no colour.
func activePaint(w io.Writer) string {
	stops, _ := theme.EmbeddedTheme("minimal")
	if len(stops) == 0 || !ansi.Enabled(w) {
		return ""
	}
	return ansi.FgSeq(theme.Look{}.Mode(), stops[0], false)
}

// renderText prints the listing to w, rows clipped on a terminal, active rows painted when w is a colour terminal.
func renderText(w io.Writer, tree *Tree, now time.Time) error {
	ew := &textutil.ErrWriter{W: w}
	renderTree(ew, tree, now, rowWidth(w), activePaint(w))
	return ew.Err
}

// renderTree writes the active sessions and jobs, a blank line, then the inactive sessions, each grouped by repo.
// Rows clip to width (0 = whole) before paint (an SGR prefix, "" = none) wraps the active rows' state cells.
func renderTree(w io.Writer, tree *Tree, now time.Time, width int, paint string) {
	if len(tree.Sessions) == 0 && len(tree.Dispatches) == 0 {
		fmt.Fprintln(w, "agents tree: no sessions in scope")
		return
	}
	home, _ := os.UserHomeDir()
	var l layout
	l.active = true
	addRepos(&l, tree.Sessions, true, now, home)
	addJobs(&l, tree.Dispatches, now)
	l.active = false
	addRepos(&l, tree.Sessions, false, now, home)
	for _, line := range l.lines(width, paint) {
		fmt.Fprintln(w, line)
	}
}

// addRepos adds a block per repo of the active or the inactive sessions, each after a blank line unless first.
func addRepos(l *layout, sessions []Session, active bool, now time.Time, home string) {
	for _, repo := range groupByRepo(sessions, active) {
		l.sep()
		l.free(0, repo.cwd)
		for _, s := range repo.sessions {
			addSession(l, s, now, home)
		}
	}
}

// isActive: the session is busy (or written recently), or anything in its subtree is running; stale never counts.
func (s Session) isActive() bool {
	if s.Status == statusActive || slices.ContainsFunc(s.Subagents, Subagent.running) {
		return true
	}
	hot := false
	for _, run := range s.Workflows {
		hot = hot || run.Status == statusRunning
		forEachRunAgent(run, func(sub Subagent) { hot = hot || sub.running() })
	}
	return hot
}

// running: the subagent, or a dispatch it launched, is still running.
func (sub Subagent) running() bool {
	live := func(d Dispatch) bool { return d.Status == statusRunning }
	return sub.Status == statusRunning || slices.ContainsFunc(sub.Dispatches, live)
}

// addJobs adds the machine-wide running dispatch jobs under a "dispatches" header.
func addJobs(l *layout, dispatches []Dispatch, now time.Time) {
	if len(dispatches) == 0 {
		return
	}
	l.sep()
	l.free(0, "dispatches")
	for _, n := range nestDispatches(dispatches) { // the id already ends in the agent name; where it runs and on what model come next
		d := n.Dispatch
		where := cmp.Or(d.Route, d.Provider, providers.AgentRecord(d.Agent).Profile, "own login") // no overlay provider: its pinned profile
		id := d.ID
		if n.level > 0 {
			id = strings.Repeat("  ", n.level-1) + "↳ " + id
		}
		l.add(levelJob, id, where, strings.TrimPrefix(cmp.Or(d.Model, "default"), "databricks-"), durationText(d.Duration),
			signalText(d, now), joinText(n.tokenText(), "  ", oneLine(cmp.Or(d.Task, d.Repo))))
		l.mark(d.Status == statusRunning, 1<<0)
	}
}

// repoGroup is one cwd's sessions, newest first.
type repoGroup struct {
	cwd      string
	sessions []Session
}

// groupByRepo buckets the active or the inactive sessions by cwd ("(unknown)" if unrecorded), in the order given.
func groupByRepo(sessions []Session, active bool) []repoGroup {
	order := []string{}
	byCwd := map[string][]Session{}
	for _, s := range sessions {
		if s.isActive() != active {
			continue
		}
		cwd := s.Cwd
		if cwd == "" {
			cwd = "(unknown)"
		}
		if _, ok := byCwd[cwd]; !ok {
			order = append(order, cwd)
		}
		byCwd[cwd] = append(byCwd[cwd], s)
	}
	groups := make([]repoGroup, 0, len(order))
	for _, cwd := range order {
		groups = append(groups, repoGroup{cwd: cwd, sessions: byCwd[cwd]})
	}
	return groups
}

// addSession adds one session row, then its subagents and workflow runs.
func addSession(l *layout, s Session, now time.Time, home string) {
	title := oneLine(s.Title)
	if title == "" {
		title = "(no prompt yet)"
	}
	l.add(levelSession, cmp.Or(s.Name, treeShortID(s.ID)), sessionStatusText(s), age(s.Updated, now), s.Branch, title)
	l.mark(s.Status == statusActive, 1<<1)
	for _, sub := range s.Subagents {
		addSubagent(l, sub, now, home, s.Cwd)
	}
	for _, run := range s.Workflows {
		addWorkflowRun(l, run, now, home, s.Cwd)
	}
}

// sessionStatusText shows the CLI's own idle|busy|waiting for a live session, else the transcript status.
func sessionStatusText(s Session) string {
	if s.Live && s.CLIStatus != "" {
		return s.CLIStatus
	}
	return s.Status
}

// statusGlyph renders a Subagent.Status as a single-character marker for a --watch-sized glance.
func statusGlyph(status string) string {
	switch status {
	case statusRunning:
		return "●"
	case statusFinished:
		return "✓"
	case statusError:
		return "✗"
	case statusStale:
		return "·"
	default:
		return " "
	}
}

// idleSince renders how long since a subagent last did anything (newest activity, else mtime); "-" if unknown.
func idleSince(sub Subagent, now time.Time) string {
	t := sub.Updated
	if sub.Activity != nil && !sub.Activity.At.IsZero() {
		t = sub.Activity.At
	}
	if t.IsZero() {
		return "-"
	}
	return shortDuration(now.Sub(t))
}

// shortDuration renders d as "49s", "7m49s" or "1h45m": whole seconds under an hour, whole minutes above.
func shortDuration(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	h, m, s := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case m > 0 && s > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", s)
}

// durationText shortens a record's Go duration string ("15m30.472s"); text that does not parse is kept as is.
func durationText(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil {
		return s
	}
	return shortDuration(d)
}

// addSubagent adds one subagent row, then a row per dispatch; deeper spawns indent inside the marker cell.
func addSubagent(l *layout, sub Subagent, now time.Time, home, cwd string) {
	nest := strings.Repeat("  ", max(sub.Depth, 1)-1)
	l.add(levelSubagent, nest+statusGlyph(sub.Status), sub.Type, sub.Status,
		"↑"+textutil.CompactCount(sub.Tokens.In), "↓"+textutil.CompactCount(sub.Tokens.Out),
		age(sub.Updated, now), idleSince(sub, now), "tool="+cmp.Or(sub.LastTool, "-"),
		oneLine(stripPaths(sub.Description, home, cwd)))
	l.mark(sub.Status == statusRunning, 1<<0|1<<2)
	for _, n := range nestDispatches(sub.Dispatches) {
		cells := dispatchCells(n.Dispatch, now)
		cells[len(cells)-1] = joinText(cells[len(cells)-1], " · ", n.tokenText())
		l.add(levelDispatch, append([]string{nest + strings.Repeat("  ", n.level) + "↳"}, cells...)...)
		l.mark(n.Status == statusRunning, 1<<2)
	}
}

// dispatchCells is a dispatch row after its marker: agent, state, duration, signal ("last=9s", "⚠ quiet 7m"), model.
func dispatchCells(d Dispatch, now time.Time) []string {
	name := d.Agent
	if d.Fallback != "" {
		name = d.Fallback + "→" + d.Agent
	}
	signal := signalText(d, now)
	if d.Fallback != "" {
		signal = strings.TrimSpace(signal + " ⚠ fallback")
	}
	return []string{name, d.Status, durationText(d.Duration), signal, d.Model}
}

// phaseDone counts a phase's agents that have a journal/meta result in, against its total.
func phaseDone(agents []Subagent) (done, total int) {
	for _, a := range agents {
		total++
		if finished(a) {
			done++
		}
	}
	return done, total
}

// addWorkflowRun adds a run header then its phases -> agents, each phase showing done/total.
func addWorkflowRun(l *layout, run WorkflowRun, now time.Time, home, cwd string) {
	l.add(levelRun, "workflow "+run.Name, run.Status, age(run.Updated, now), run.ID)
	l.mark(run.Status == statusRunning, 1<<1)
	for _, phase := range run.Phases {
		done, total := phaseDone(phase.Agents)
		l.free(6, fmt.Sprintf("phase %s (%d/%d done)", phase.Name, done, total))
		for _, sub := range phase.Agents {
			addSubagent(l, sub, now, home, cwd)
		}
	}
}

// age renders how long ago t was, "-" when t is zero.
func age(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// treeShortID is the id's first 8 characters, enough to disambiguate without blowing the 80-column budget.
func treeShortID(id string) string {
	r := []rune(id)
	if len(r) <= 8 {
		return id
	}
	return string(r[:8])
}

// View is how `agents tree` shows the listing: JSON or text, every entry or only running work, once or redrawn.
type View struct {
	JSON, All, Watch bool
	Interval         time.Duration
}

// Show renders the tree once, or with v.Watch redraws it in place every v.Interval until a render fails.
func Show(claudeDir string, opts Options, v View) int {
	var f *frame
	if v.Watch {
		opts.scans = loadScanCache()
		if rowWidth(os.Stdout) > 0 {
			f = &frame{paint: activePaint(os.Stdout)}
		}
	}
	for {
		if v.Watch && f == nil {
			fmt.Print("\x1b[H\x1b[2J") // piped: no in-place redraw, keep wiping between frames
		}
		if code := show(claudeDir, opts, v, f); code != 0 || !v.Watch {
			return code
		}
		time.Sleep(v.Interval)
	}
}

// show lists the tree once and writes it as JSON or text, or as a watch frame when f is set.
func show(claudeDir string, opts Options, v View, f *frame) int {
	listing, err := listing(claudeDir, opts, v.All)
	if err == nil && v.JSON {
		err = helptext.WriteJSON(os.Stdout, listing)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agents tree:", err)
		return 1
	}
	if v.JSON {
		return 0
	}
	if f != nil {
		f.width = rowWidth(os.Stdout)
		err = f.draw(os.Stdout, listing, time.Now())
	} else {
		err = renderText(os.Stdout, listing, time.Now())
	}
	if err != nil {
		return helptext.Fail("agents tree: %v", err)
	}
	return 0
}

// frame is a watch redraw built in memory, its buffers reused across ticks.
type frame struct {
	text, out bytes.Buffer
	width     int
	paint     string
}

// draw writes one frame in a single write: cursor home, each row cleared to its end, then the rest of the screen.
func (f *frame) draw(w io.Writer, tree *Tree, now time.Time) error {
	f.text.Reset()
	f.out.Reset()
	renderTree(&f.text, tree, now, f.width, f.paint)
	f.out.WriteString("\x1b[H")
	for rest := f.text.Bytes(); len(rest) > 0; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			i = len(rest)
		}
		f.out.Write(rest[:i])
		f.out.WriteString("\x1b[K")
		if i < len(rest) {
			f.out.WriteByte('\n')
			i++
		}
		rest = rest[i:]
	}
	f.out.WriteString("\x1b[J")
	_, err := w.Write(f.out.Bytes())
	return err
}

// nested is a dispatch placed under its parent: level is its depth in the list, sum its subtree's tokens.
type nested struct {
	Dispatch
	level    int
	children bool
	sum      int64
}

// tokens is the dispatch's own token total (input, cache and output); 0 when the record carries no usage.
func (d Dispatch) tokens() int64 {
	if d.Usage == nil {
		return 0
	}
	return int64(d.Usage.InputTokens + d.Usage.CacheCreationTokens + d.Usage.CacheReadTokens + d.Usage.OutputTokens)
}

// tokenText is "24k" for the dispatch's own tokens, plus "Σ70k" when it has children; "" with nothing to show.
func (n nested) tokenText() string {
	own := ""
	if t := n.tokens(); t > 0 {
		own = textutil.CompactCount(t)
	}
	if !n.children || n.sum == 0 {
		return own
	}
	return joinText(own, " ", "Σ"+textutil.CompactCount(n.sum))
}

// joinText is a and b around sep, or just the one that is not empty.
func joinText(a, sep, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + sep + b
}

// nestDispatches orders ds so each child follows its parent; a parent outside ds, or a cycle, leaves a root.
func nestDispatches(ds []Dispatch) []nested {
	byID := make(map[string]int, len(ds))
	for i, d := range ds {
		byID[d.ID] = i
	}
	kids, roots := map[int][]int{}, []int{}
	for i, d := range ds {
		if p, ok := byID[d.Parent]; ok && d.Parent != "" && p != i {
			kids[p] = append(kids[p], i)
		} else {
			roots = append(roots, i)
		}
	}
	out, seen := make([]nested, 0, len(ds)), make([]bool, len(ds))
	var walk func(i, level int) int64
	walk = func(i, level int) int64 {
		seen[i] = true
		at := len(out)
		out = append(out, nested{Dispatch: ds[i], level: level})
		sum := ds[i].tokens()
		for _, k := range kids[i] {
			if !seen[k] {
				out[at].children = true
				sum += walk(k, level+1)
			}
		}
		out[at].sum = sum
		return sum
	}
	for _, i := range roots {
		walk(i, 0)
	}
	for i := range ds { // members of a parent cycle have no root
		if !seen[i] {
			walk(i, 0)
		}
	}
	return out
}
