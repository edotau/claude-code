// render.go renders the tree data for the terminal.

package tree

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// defaultWidth is the render width when stdout is not a terminal and COLUMNS is unset.
const defaultWidth = 120

// terminalWidth is $COLUMNS when set, else the stdout terminal's width, else defaultWidth.
func terminalWidth() int {
	if n := ansi.Columns(0); n > 0 {
		return n
	}
	if n, _, err := proc.Size(int(os.Stdout.Fd())); err == nil && n > 0 {
		return n
	}
	return defaultWidth
}

// columnWidths sizes the type/status columns to the longest rendered value, not a fixed guess like "general-purpose".
type columnWidths struct {
	subType    int
	subStatus  int
	sessStatus int
	sessLabel  int // 0 when no session carries a registry name, keeping the bare treeShortID layout
}

// computeColumnWidths walks every subagent in the tree (direct and nested inside workflow runs).
func computeColumnWidths(tree *Tree) columnWidths {
	c := columnWidths{subType: 4, subStatus: 6, sessStatus: 6} // "type"/"status"-ish floors
	grow := func(sub Subagent) {
		c.subType = max(c.subType, textutil.RuneLen(sub.Type))
		c.subStatus = max(c.subStatus, textutil.RuneLen(sub.Status))
	}
	for _, s := range tree.Sessions {
		c.sessStatus = max(c.sessStatus, textutil.RuneLen(sessionStatusText(s)))
		if s.Name != "" {
			c.sessLabel = max(c.sessLabel, 8, min(textutil.RuneLen(s.Name), maxSessionLabel))
		}
		for _, sub := range s.Subagents {
			grow(sub)
		}
		for _, run := range s.Workflows {
			forEachRunAgent(run, grow)
		}
	}
	return c
}

// renderText groups sessions by repo (newest first), indents subagents by spawnDepth, fits rows to the terminal width.
func renderText(w io.Writer, tree *Tree, now time.Time) error {
	ew := &textutil.ErrWriter{W: w}
	renderTree(ew, tree, now)
	return ew.Err
}

// renderTree writes the grouped listing to w; write errors are collected by the textutil.ErrWriter it is given.
func renderTree(w io.Writer, tree *Tree, now time.Time) {
	if len(tree.Sessions) == 0 && len(tree.Dispatches) == 0 {
		fmt.Fprintln(w, "agents tree: no sessions in scope")
		return
	}
	width := terminalWidth()
	cols := computeColumnWidths(tree)
	home, _ := os.UserHomeDir()
	for _, repo := range groupByRepo(tree.Sessions) {
		fmt.Fprintf(w, "%s\n", repo.cwd)
		for _, s := range repo.sessions {
			renderSession(w, s, now, cols, width, home)
		}
		fmt.Fprintln(w)
	}
	renderDispatches(w, tree.Dispatches, now, width)
}

// renderDispatches prints the machine-wide running dispatch jobs, one row each, under a "dispatches" header.
func renderDispatches(w io.Writer, dispatches []Dispatch, now time.Time, width int) {
	if len(dispatches) == 0 {
		return
	}
	fmt.Fprintln(w, "dispatches")
	for _, l := range dispatches { // the id already ends in the agent name; where it runs and on what model come next
		where := cmp.Or(l.Route, l.Provider, providers.AgentRecord(l.Agent).Profile, "own login") // no overlay provider: its pinned profile
		prefix := fmt.Sprintf("  %s  %-15s  %-22s  %-6s", l.ID, where, strings.TrimPrefix(cmp.Or(l.Model, "default"), "databricks-"), l.Duration)
		if sig := signalText(l, now); sig != "" {
			prefix += "  " + sig
		}
		fmt.Fprintln(w, fitRow(prefix, cmp.Or(l.Task, l.Repo), "", width))
	}
}

// repoGroup is one cwd's sessions, newest first.
type repoGroup struct {
	cwd      string
	sessions []Session
}

// groupByRepo buckets sessions by cwd ("(unknown)" if unrecorded), newest first within a bucket and by newest session.
func groupByRepo(sessions []Session) []repoGroup {
	order := []string{}
	byCwd := map[string][]Session{}
	for _, s := range sessions {
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

// renderSession prints one session line plus its branch/age, then its subagents and workflow runs.
func renderSession(w io.Writer, s Session, now time.Time, cols columnWidths, width int, home string) {
	title := s.Title
	if title == "" {
		title = "(no prompt yet)"
	}
	prefix := fmt.Sprintf("  %s  %-*s  %-6s  %-8s", sessionLabel(s, cols.sessLabel), cols.sessStatus,
		sessionStatusText(s), age(s.Updated, now), s.Branch)
	fmt.Fprintln(w, fitRow(prefix, title, "", width))
	for _, sub := range s.Subagents {
		renderSubagent(w, sub, now, cols, width, home, s.Cwd)
	}
	for _, run := range s.Workflows {
		renderWorkflowRun(w, run, now, cols, width, home, s.Cwd)
	}
}

// maxSessionLabel caps the session-name column so a long registry name can't crowd out the title.
const maxSessionLabel = 24

// sessionLabel is the row's lead column: the registry name when set, else treeShortID, padded to width runes.
func sessionLabel(s Session, width int) string {
	label := treeShortID(s.ID)
	if width == 0 {
		return label
	}
	if s.Name != "" {
		label = s.Name
		if textutil.RuneLen(label) > width {
			label = string([]rune(label)[:width-1]) + "…"
		}
	}
	return label + strings.Repeat(" ", width-textutil.RuneLen(label))
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
	return now.Sub(t).Round(time.Second).String()
}

// renderSubagent prints one subagent line indented per spawn depth, then a row per dispatch; fitRow drops tool first.
func renderSubagent(w io.Writer, sub Subagent, now time.Time, cols columnWidths, width int, home, cwd string) {
	indent := strings.Repeat("  ", max(sub.Depth, 1)+1)
	tool := sub.LastTool
	if tool == "" {
		tool = "-"
	}
	prefix := fmt.Sprintf("%s%s %-*s %-*s depth=%d  %s  %s  idle=%s",
		indent, statusGlyph(sub.Status), cols.subType, sub.Type, cols.subStatus, sub.Status, sub.Depth,
		tokenCompact(sub.Tokens), age(sub.Updated, now), idleSince(sub, now))
	label := stripPaths(sub.Description, home, cwd)
	fmt.Fprintln(w, fitRow(prefix, truncate(label, 60), tool, width))
	for _, disp := range sub.Dispatches {
		fmt.Fprintf(w, "%s  %s\n", indent, fitRow(formatDispatch(disp, now), "", "", width))
	}
}

// formatDispatch renders one dispatch: "↳ codex gpt-6-astra running 2m14s last=9s", "… ⚠ quiet 7m" or "… ⚠ fallback".
func formatDispatch(l Dispatch, now time.Time) string {
	name := l.Agent
	if l.Fallback != "" {
		name = l.Fallback + "→" + l.Agent
	}
	status, sig := l.Status, signalText(l, now)
	if l.Quiet {
		status, sig = sig, ""
	}
	s := fmt.Sprintf("↳ %s %s %s %s", name, l.Model, status, l.Duration)
	if sig != "" {
		s += " " + sig
	}
	if l.Fallback != "" {
		s += " ⚠ fallback"
	}
	return s
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

// renderWorkflowRun prints a run header then its phases -> agents, each phase showing done/total.
func renderWorkflowRun(w io.Writer, run WorkflowRun, now time.Time, cols columnWidths, width int, home, cwd string) {
	indent := strings.Repeat("  ", 2)
	prefix := fmt.Sprintf("%sworkflow %s  %-9s %-6s", indent, run.Name, run.Status, age(run.Updated, now))
	fmt.Fprintln(w, fitRow(prefix, run.ID, "", width))
	for _, phase := range run.Phases {
		done, total := phaseDone(phase.Agents)
		fmt.Fprintf(w, "%s  phase %s (%d/%d done)\n", indent, phase.Name, done, total)
		for _, sub := range phase.Agents {
			renderSubagent(w, sub, now, cols, width, home, cwd)
		}
	}
}

// fitRow composes "prefix  tail  tool=X" and fits it to width: the tool segment is dropped first, then tail truncated.
func fitRow(prefix, tail, tool string, width int) string {
	toolPart := ""
	if tool != "" {
		toolPart = "  tool=" + tool
	}
	line := joinNonEmpty(prefix, tail) + toolPart
	if textutil.RuneLen(line) <= width {
		return line
	}
	// Why: the description outranks the last tool, so the tool segment goes before the description shrinks.
	line = joinNonEmpty(prefix, tail)
	if textutil.RuneLen(line) > width && tail != "" {
		tail = shrink(tail, textutil.RuneLen(line)-width)
		line = joinNonEmpty(prefix, tail)
	}
	return textutil.TruncateRunes(line, width)
}

// joinNonEmpty joins prefix and tail with two spaces, skipping the separator when tail is empty.
func joinNonEmpty(prefix, tail string) string {
	if tail == "" {
		return prefix
	}
	return prefix + "  " + tail
}

// shrink cuts n runes (plus the ellipsis itself) off the end of s, floored at an empty string.
func shrink(s string, n int) string {
	r := []rune(strings.TrimSuffix(s, "…"))
	keep := len(r) - n - 1 // -1 reserves room for the ellipsis this re-adds
	if keep <= 0 {
		return ""
	}
	return string(r[:keep]) + "…"
}

// tokenCompact renders in/out tokens as "in=12.3k out=4.1k".
func tokenCompact(t Tokens) string {
	return fmt.Sprintf("in=%s out=%s", textutil.CompactCount(t.In), textutil.CompactCount(t.Out))
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

// Show renders the tree once, or with v.Watch clears and redraws every v.Interval until a render fails.
func Show(claudeDir string, opts Options, v View) int {
	for {
		if v.Watch {
			fmt.Print("\x1b[H\x1b[2J") // re-home the cursor and wipe the terminal between redraws
		}
		if code := show(claudeDir, opts, v); code != 0 || !v.Watch {
			return code
		}
		time.Sleep(v.Interval)
	}
}

// show lists the tree once and writes it as JSON or text.
func show(claudeDir string, opts Options, v View) int {
	listing, err := listing(claudeDir, opts, v.All)
	if err == nil && v.JSON {
		err = helptext.WriteJSON(os.Stdout, listing)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agents tree:", err)
		return 1
	}
	if !v.JSON {
		if err := renderText(os.Stdout, listing, time.Now()); err != nil {
			return helptext.Fail("agents tree: %v", err)
		}
	}
	return 0
}
