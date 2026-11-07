// The goal-keeper: a Stop that blocks while this session has open goals whose done check still fails.
package agents

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/goal"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// goalBudget leaves headroom inside the hook's 5s budget for the ticket writes.
const goalBudget = 4 * time.Second

// workflowRunRe finds the run id a Workflow launch result names.
var workflowRunRe = regexp.MustCompile(`Run ID: (wf_[\w-]+)`)

// registerWorkflow files a goal (done = run completed) for the Workflow launch this PostToolUse reports.
func registerWorkflow(in *core.Input) {
	if in.ToolName != "Workflow" || in.SessionID == "" || in.TranscriptPath == "" {
		return
	}
	m := workflowRunRe.FindSubmatch(in.Raw)
	if m == nil {
		return
	}
	run := string(m[1])
	retireResumed(in, run)
	for _, g := range goal.All() {
		if g.Run == run && g.Session == in.SessionID {
			return
		}
	}
	text := "workflow run " + run
	if name := filepath.Base(in.Nested("tool_input", "scriptPath")); name != "." && name != "" {
		text = "workflow " + strings.TrimSuffix(name, ".js") + " " + run
	}
	_, _ = goal.Add(goal.Goal{Text: text, Run: run, Session: in.SessionID, CWD: in.CWD,
		SessionDir: strings.TrimSuffix(in.TranscriptPath, ".jsonl")})
}

// retireResumed closes the open goal of the run this launch resumes; resuming under the same id re-arms it instead.
func retireResumed(in *core.Input, run string) {
	old := in.Nested("tool_input", "resumeFromRunId")
	if old == "" {
		return
	}
	for _, g := range goal.All() {
		if g.Run != old || g.Session != in.SessionID || g.Status != goal.Open {
			continue
		}
		if old == run {
			g.LastHash, g.Blocks = "", 0
			_ = goal.Save(g)
			continue
		}
		_, _ = goal.Tick(g, goal.Closed, "resumed as "+run, g.LastOut)
	}
}

// GoalKeeper evaluates this session's open goals: passes tick done, a failure blocks the stop, a repeat stalls it.
func GoalKeeper(in *core.Input) core.Result {
	if in.SessionID == "" {
		return core.Result{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), goalBudget)
	defer cancel()
	blocked, notes := keepAll(ctx, in)
	res := core.Result{SystemMessage: strings.Join(notes, "\n")}
	if len(blocked) > 0 {
		res.Decision = core.Block
		res.Reason = strings.Join(blocked, "\n") + "\nResume (Workflow resumeFromRunId or re-dispatch), then re-check."
	}
	return res
}

// keepAll ticks this session's open goals; other sessions' goals are only listed.
func keepAll(ctx context.Context, in *core.Input) (blocked, notes []string) {
	for _, g := range goal.All() {
		switch {
		case g.Status != goal.Open:
		case g.Session != in.SessionID:
			notes = append(notes, fmt.Sprintf("%s — other session %s, not blocking", g.Line(), g.Session))
		default:
			line, block := keepGoal(ctx, g)
			if block {
				blocked = append(blocked, line)
			} else {
				notes = append(notes, line)
			}
		}
	}
	return blocked, notes
}

// keepGoal ticks one goal of this session and returns its tick line and whether it blocks the stop.
func keepGoal(ctx context.Context, g goal.Goal) (string, bool) {
	if goal.PlanChecked(g) {
		g, _ = goal.Tick(g, goal.Done, "plan line checked off", g.LastOut)
		return g.Line() + " — done (plan line checked)", false
	}
	v := goal.Evaluate(ctx, g)
	switch {
	case v.Pass:
		g, _ = goal.Tick(g, goal.Done, "check passed", v.Out)
		return g.Line() + " — done", false
	case v.Pending:
		g, _ = goal.Tick(g, goal.Open, "", v.Out)
		return g.Line() + " — pending: " + goal.Tail(v.Out), false
	case goal.Hash(goal.Tail(v.Out)) == g.LastHash:
		g, _ = goal.Tick(g, goal.Stalled, "same failing output twice", v.Out)
		return g.Line() + " — stalled on the same failure, ask the user: " + goal.Tail(v.Out), false
	case g.Blocks >= goal.MaxBlocks:
		g, _ = goal.Tick(g, goal.Stalled, fmt.Sprintf("still failing after %d blocks", g.Blocks), v.Out)
		return g.Line() + " — stalled after " + strconv.Itoa(goal.MaxBlocks) + " blocks, ask the user: " + goal.Tail(v.Out), false
	}
	g.LastHash, g.Blocks = goal.Hash(goal.Tail(v.Out)), g.Blocks+1
	g, _ = goal.Tick(g, goal.Open, "", v.Out)
	return fmt.Sprintf("goal %s still open: %s — last error: %s", g.ID, g.Line(), goal.Tail(v.Out)), true
}
