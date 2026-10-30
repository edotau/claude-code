// Live plan-task claims for the dispatch group: a spawn brief naming a task claims it; the agent's stop clears it.
package hooks

import (
	"cmp"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/worktree"
)

var (
	briefTaskRe = regexp.MustCompile(`(?mi)^[\s>*-]*Task:\s*(\S.*?)\s*$`)
	nonWordRe   = regexp.MustCompile(`[^\p{L}\p{N}.]+`)
)

// claimPlanTask claims the active plan's task a spawn names (a `Task:` brief line, else a unique checklist title
// the description matches); no confident match, no plan or any error claims nothing.
func claimPlanTask(in *Input, id string) {
	if !spawnTools[in.ToolName] || id == "" {
		return
	}
	plan, worktreeName, ok := activePlan(in.CWD)
	if !ok {
		return
	}
	parsed, err := plans.Parse(plan)
	if err != nil {
		return
	}
	task, ok := matchBriefTask(parsed, in.nested("tool_input", "prompt"), in.nested("tool_input", "description"))
	if !ok || task.Claim != "" { // the locked ClaimFree below re-checks
		return
	}
	agent := cmp.Or(in.nested("tool_input", "subagent_type"), in.ToolName)
	text := fmt.Sprintf("working: %s %s · worktree %s · since %s", agent, id, cmp.Or(worktreeName, "main"), time.Now().Format("15:04"))
	if plans.ClaimFree(plan, taskKey(task), text) != nil || worktreeName == "" {
		return
	}
	_, _ = worktree.SetLedgerTask(in.CWD, taskKey(task)+" in "+strings.TrimSuffix(filepath.Base(plan), ".md"), time.Now())
}

// clearPlanClaim removes the claim a stopping agent's spawn wrote, found by its tool_use_id in the claim text.
func clearPlanClaim(cwd, id string) {
	if id == "" {
		return
	}
	plan, _, ok := activePlan(cwd)
	if !ok {
		return
	}
	parsed, err := plans.Parse(plan)
	if err != nil {
		return
	}
	for _, t := range parsed.Tasks {
		if strings.Contains(t.Claim, " "+id+" ") {
			_ = plans.ClearClaimOwned(plan, taskKey(t), " "+id+" ")
		}
	}
}

// activePlan is cwd's repo plan with an open owned task, plus the linked worktree's name ("" in the main checkout).
func activePlan(cwd string) (plan, worktreeName string, ok bool) {
	top := memory.GitToplevel(cwd)
	if top == "" {
		return "", "", false
	}
	if paths.MainCheckout(top) != top {
		worktreeName = filepath.Base(top)
	}
	plan, ok = plans.Active(plans.Home(top), worktreeName, MirrorDests())
	return plan, worktreeName, ok
}

func taskKey(t plans.Task) string { return cmp.Or(t.ID, t.Title) }

func normTask(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "phase ")
	return strings.Trim(nonWordRe.ReplaceAllString(s, " "), " .")
}

// matchBriefTask returns the one open task a `Task:` line names by id, else the one a description matches by title.
func matchBriefTask(p plans.Plan, brief, description string) (plans.Task, bool) {
	if m := briefTaskRe.FindStringSubmatch(brief); m != nil {
		want := normTask(m[1])
		var hit []plans.Task
		for _, t := range p.Tasks {
			if !t.Done && want != "" && (normTask(t.ID) == want || normTask(t.Title) == want) {
				hit = append(hit, t)
			}
		}
		return only(hit)
	}
	want := normTask(description)
	if len(strings.Fields(want)) < 3 {
		return plans.Task{}, false
	}
	var hit []plans.Task
	for _, t := range p.Tasks {
		if title := normTask(t.Title); !t.Done && (title == want || strings.Contains(title, want)) {
			hit = append(hit, t)
		}
	}
	return only(hit)
}

func only(ts []plans.Task) (plans.Task, bool) {
	if len(ts) != 1 {
		return plans.Task{}, false
	}
	return ts[0], true
}

// clearStoppedClaim resolves the stopping agent's spawn id from its meta.json and clears that spawn's plan claim.
func clearStoppedClaim(in *Input, dirs []string, id string) {
	for _, d := range dirs {
		if tu := inflight.ToolUseID(d, id); tu != "" {
			clearPlanClaim(in.CWD, tu)
			return
		}
	}
}
