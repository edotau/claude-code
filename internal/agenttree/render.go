package agenttree

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultWidth is the fallback render width when COLUMNS isn't set (or isn't a usable int) — widest
// value that still reads well in a plain terminal; --watch callers inherit the real COLUMNS instead.
const defaultWidth = 120

// terminalWidth reads COLUMNS (the shell exports it on an interactive tty; --watch callers inherit
// it too), falling back to defaultWidth so a redirected/piped run still wraps sanely.
func terminalWidth() int {
	if v := strings.TrimSpace(os.Getenv("COLUMNS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultWidth
}

// columnWidths sizes the type/status columns to the longest value actually in the rendered set, so
// a table stays aligned instead of a fixed guess (e.g. "code-worker") breaking it.
type columnWidths struct {
	subType    int
	subStatus  int
	sessStatus int
}

// computeColumnWidths walks every subagent in the tree (direct and nested inside workflow runs).
func computeColumnWidths(tree *Tree) columnWidths {
	c := columnWidths{subType: 4, subStatus: 6, sessStatus: 6} // "type"/"status"-ish floors
	grow := func(sub Subagent) {
		c.subType = max(c.subType, len(sub.Type))
		c.subStatus = max(c.subStatus, len(sub.Status))
	}
	for _, s := range tree.Sessions {
		c.sessStatus = max(c.sessStatus, len(s.Status))
		for _, sub := range s.Subagents {
			grow(sub)
		}
		for _, run := range s.Workflows {
			for _, phase := range run.Phases {
				for _, sub := range phase.Agents {
					grow(sub)
				}
			}
		}
	}
	return c
}

// RenderText groups tree.Sessions by repo (cwd), newest session first within each repo, and prints
// each session's subagents indented by spawnDepth, with Workflow runs shown as phases -> agents.
// Every row is fit to terminalWidth() by truncating its description (then its tool, as a last
// resort) rather than wrapping.
func RenderText(w io.Writer, tree *Tree, now time.Time) {
	if len(tree.Sessions) == 0 {
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
}

// repoGroup is one cwd's sessions, newest first.
type repoGroup struct {
	cwd      string
	sessions []Session
}

// groupByRepo buckets sessions by cwd ("(unknown)" when the transcript never recorded one),
// preserving the overall newest-first order within each bucket and ordering buckets by their
// newest session.
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
	prefix := fmt.Sprintf("  %s  %-*s  %-6s  %-8s", shortID(s.ID), cols.sessStatus, s.Status, age(s.Updated, now), s.Branch)
	fmt.Fprintln(w, fitRow(prefix, title, "", width))
	for _, sub := range s.Subagents {
		renderSubagent(w, sub, now, cols, width, home, s.Cwd)
	}
	for _, run := range s.Workflows {
		renderWorkflowRun(w, run, now, cols, width, home, s.Cwd)
	}
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

// idleSince renders how long since a subagent last did anything: its newest activity timestamp when
// known, else its transcript's own mtime; "-" when neither is available.
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

// renderSubagent prints one subagent line (status glyph, idle time), indented two columns per spawn
// depth beyond the session, then one indented row per vendor leg it launched. Description is
// rendered before tool so, when the row still overflows after truncating the description, it's the
// trailing tool field that gets dropped — the least important part.
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
	for _, leg := range sub.Legs {
		fmt.Fprintf(w, "%s  %s\n", indent, formatLeg(leg))
	}
}

// formatLeg renders one vendor dispatch: "↳ codex gpt-6-astra running 2m14s", or with a fallback,
// "↳ codex→claude ok 1m40s ⚠ fallback".
func formatLeg(l Leg) string {
	name := l.Agent
	if l.Fallback != "" {
		name = l.Fallback + "→" + l.Agent
	}
	s := fmt.Sprintf("↳ %s %s %s %s", name, l.Model, l.Status, l.Duration)
	if l.Fallback != "" {
		s += " ⚠ fallback"
	}
	return s
}

// phaseDone counts a phase's agents that have a journal/meta result in, against its total.
func phaseDone(agents []Subagent) (done, total int) {
	for _, a := range agents {
		total++
		if a.Status == statusFinished || a.Status == statusError {
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

// fitRow composes "prefix  tail  tool=X" (tail and the tool segment both optional) and fits it to
// width: tail is truncated with an ellipsis first, then the tool segment is dropped outright if the
// row still doesn't fit — the two least important parts, trimmed in that order.
func fitRow(prefix, tail, tool string, width int) string {
	toolPart := ""
	if tool != "" {
		toolPart = "  tool=" + tool
	}
	line := joinNonEmpty(prefix, tail) + toolPart
	if runeLen(line) <= width {
		return line
	}
	// Why: the description outranks the last tool, so the tool segment goes before the description shrinks.
	line = joinNonEmpty(prefix, tail)
	if runeLen(line) > width && tail != "" {
		tail = shrink(tail, runeLen(line)-width)
		line = joinNonEmpty(prefix, tail)
	}
	return clip(line, width)
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

// clip hard-cuts s to width runes, no ellipsis (used only on the already-shrunk fallback line).
func clip(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width])
}

func runeLen(s string) int { return len([]rune(s)) }

// tokenCompact renders in/out tokens as "12.3k/4.1k" style, zero-padded short form.
func tokenCompact(t Tokens) string {
	return fmt.Sprintf("in=%s out=%s", compactNumber(t.In), compactNumber(t.Out))
}

// compactNumber renders n with a k/M suffix past 1000/1e6, one decimal.
func compactNumber(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return strconv.FormatInt(n, 10)
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

// shortID is the session/agent id's first 8 characters, long enough to disambiguate in practice
// without blowing the 80-column budget.
func shortID(id string) string {
	r := []rune(id)
	if len(r) <= 8 {
		return id
	}
	return string(r[:8])
}
