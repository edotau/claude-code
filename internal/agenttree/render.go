package agenttree

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// RenderText groups tree.Sessions by repo (cwd), newest session first within each repo, and prints
// each session's subagents indented by spawnDepth, with Workflow runs shown as phases -> agents.
func RenderText(w io.Writer, tree *Tree, now time.Time) {
	if len(tree.Sessions) == 0 {
		fmt.Fprintln(w, "agents tree: no sessions in scope")
		return
	}
	for _, repo := range groupByRepo(tree.Sessions) {
		fmt.Fprintf(w, "%s\n", repo.cwd)
		for _, s := range repo.sessions {
			renderSession(w, s, now)
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
func renderSession(w io.Writer, s Session, now time.Time) {
	title := s.Title
	if title == "" {
		title = "(no prompt yet)"
	}
	fmt.Fprintf(w, "  %s  %-7s  %-6s  %-8s  %s\n", shortID(s.ID), s.Status, age(s.Updated, now), s.Branch, title)
	for _, sub := range s.Subagents {
		renderSubagent(w, sub, now)
	}
	for _, run := range s.Workflows {
		renderWorkflowRun(w, run, now)
	}
}

// renderSubagent prints one subagent line, indented two columns per spawn depth beyond the session.
func renderSubagent(w io.Writer, sub Subagent, now time.Time) {
	indent := strings.Repeat("  ", max(sub.Depth, 1)+1)
	desc := truncate(sub.Description, 60)
	tool := sub.LastTool
	if tool == "" {
		tool = "-"
	}
	fmt.Fprintf(w, "%s%-10s %-8s depth=%d  %s  %s  tool=%s  %s\n",
		indent, sub.Type, sub.Status, sub.Depth, tokenCompact(sub.Tokens), age(sub.Updated, now), tool, desc)
}

// renderWorkflowRun prints a run header then its phases -> agents.
func renderWorkflowRun(w io.Writer, run WorkflowRun, now time.Time) {
	indent := strings.Repeat("  ", 2)
	status := run.Status
	if status == "" {
		status = "running"
	}
	fmt.Fprintf(w, "%sworkflow %s  %-9s %-6s  %s\n", indent, run.Name, status, age(run.Updated, now), run.ID)
	for _, phase := range run.Phases {
		fmt.Fprintf(w, "%s  phase %s\n", indent, phase.Name)
		for _, sub := range phase.Agents {
			renderSubagent(w, sub, now)
		}
	}
}

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
		return fmt.Sprintf("%d", n)
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
