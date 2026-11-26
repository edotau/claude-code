package dispatchhook

import (
	"cmp"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/health"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/worktree"
)

// reportFailedJobs broadcasts each failed job and contract the result names; it returns their degraded notice, if any.
func reportFailedJobs(in *core.Input) string {
	if !core.SpawnTools[in.ToolName] && in.ToolName != "Workflow" {
		return ""
	}
	var all []string
	for _, text := range []string{cmp.Or(in.Nested("tool_response", "stdout"), in.ToolResponseText()), in.Nested("error")} {
		all = append(all, allIDs(text)...)
	}
	var notes []string
	for _, id := range slices.Compact(slices.Sorted(slices.Values(all))) {
		if id != "" {
			notes = append(notes, reportFailedJob(id))
		}
	}
	return core.JoinNonEmpty("\n", slices.Compact(slices.Sorted(slices.Values(notes)))...)
}

// allIDs is every job id a result names on a launch, exit or envelope line, not only its own head and exit.
func allIDs(text string) []string {
	var ids []string
	for l := range strings.SplitSeq(ansiRe().ReplaceAllString(text, ""), "\n") {
		if id := headID(strings.TrimSpace(l)); id != "" {
			ids = append(ids, id)
		}
	}
	return append(ids, ownIDs(text)...)
}

func reportFailedJob(id string) (note string) {
	rec, err := loadDispatch(id)
	if err == nil && (rec.Failed() || rec.Outcome() == "lost") && rec.FailKind != agent.FailCancelled {
		note = degradedNote(rec.Request.Prompt, rec.PlanTask)
		_, _ = worktree.ReportFailure(worktree.FailuresDir(), worktree.Failure{
			Kind: worktree.FailDispatch, Subject: id, Summary: cmp.Or(rec.Error, rec.Cutoff, rec.Outcome()),
			Log: filepath.Join(paths.DispatchDir(), id+".json"), Repo: rec.Repo,
		})
	}
	if c, err := contract.Load(id); err == nil && c.Failed != "" && c.Failed != contract.ReasonCancelled {
		log, _ := contract.Path(id)
		_, _ = worktree.ReportFailure(worktree.FailuresDir(), worktree.Failure{
			Kind: worktree.FailContract, Subject: id, Summary: string(c.Failed), Log: log, Repo: rec.Repo,
		})
	}
	return note
}

// degradedNote is the R34 notice for a task, "" while dispatch is healthy for it.
func degradedNote(task, planTask string) string {
	if h := agent.DispatchHealth(agent.TaskKey{Text: task, PlanTask: planTask}); h.Degraded {
		return health.Notice(h.Reason)
	}
	return ""
}

// readyNotice is the plans ready-task line for the free slots; "" prints nothing.
func readyNotice(cwd string) string {
	if degradedNote("", "") != "" {
		return "" // dispatch is down: no launch guidance
	}
	_, main := paths.CheckoutOf(cwd)
	slots := max(router.CurrentLimits().Total-inflight.Count().Total, 0)
	return plans.ReadyNotice(plans.ReadyInput{Root: cmp.Or(main, cwd), Slots: slots, Claims: liveClaims})
}

// liveClaims is every live claim, this session's own included, as claimed path to claimants.
func liveClaims() map[string][]string {
	hits, _ := worktree.LiveClaims(worktree.ClaimSource{Alive: proc.Alive, Owner: inflight.SessionOwner,
		Leaf:  func(worktree.Claim) string { return "this session" },
		Label: func(pid int) string { return worktree.SessionLabel(worktree.Proc{PID: pid, Comm: "claude"}) }})
	out := make(map[string][]string, len(hits))
	for p, hs := range hits {
		for _, h := range hs {
			out[p] = append(out[p], h.Who)
		}
	}
	return out
}

var taskLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^Task: (\S+ \S+)\s*$`) })

// briefPlanTask is the plan task ("<plan-slug> <id>") a brief's trailing Task: line names, "" when it has none.
func briefPlanTask(brief string) string {
	if m := taskLineRe().FindStringSubmatch(brief); m != nil {
		return m[1]
	}
	return ""
}
