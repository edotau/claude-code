package hooks

import (
	"cmp"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/waves"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
)

// handbackVerify runs the leaf's plan task verify: and the scoped gate on its owned dirty files, before deadline.
func handbackVerify(in *core.Input, lr leafRead, deadline time.Time) []string {
	top := memory.GitToplevel(in.CWD)
	if top == "" {
		return nil
	}
	dirty := dirtyPaths(top)
	planPath, active, _ := plans.Active(plans.Home(top), plans.WorktreeName(top), plans.MirrorDests())
	recTask := recordPlanTask(loadRecords(lr.ids))
	named, task, isNamed, ok := docs.NamedPlanTask(lr.scan.brief, recTask)
	if isNamed {
		if !ok {
			return nil
		}
		planPath = named
	} else if task, ok = docs.BriefTask(in.CWD, lr.scan.brief, recTask); !ok {
		if task, ok = untiedTask(active, dirty); !ok {
			return nil
		}
	}
	id := cmp.Or(task.ID, task.Title)
	files := dirtyOwned(dirty, plans.OwnedPaths(task))
	commit := "commit+tick: claude-code worktree commit --task '" + id + "' --plan " + planPath +
		` -m "<type>: <subject>"` + strings.Join(append([]string{""}, files...), " ")
	if time.Until(deadline) < 2*time.Second {
		return []string{"verify " + id + ": no time left in the hand-back budget; the commit-time verify still runs", commit}
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	checks := waves.VerifyTask(ctx, waves.TaskVerifyInput{Dir: top, Task: task, Files: files,
		OutDir: filepath.Join(paths.StateDir(), "verify", "handback")})
	lines := make([]string, 0, len(checks)+1)
	for _, c := range checks {
		lines = append(lines, c.Line())
	}
	return append(lines, commit)
}

// untiedTask is the one open task of the plan whose owns: covers a dirty path; several or none is no guess.
func untiedTask(plan plans.Plan, dirty []string) (plans.Task, bool) {
	var hit []plans.Task
	for _, t := range plan.Tasks {
		if !t.Done && !t.Dropped && len(dirtyOwned(dirty, plans.OwnedPaths(t))) > 0 {
			hit = append(hit, t)
		}
	}
	if len(hit) != 1 {
		return plans.Task{}, false
	}
	return hit[0], true
}

func recordPlanTask(recs []agent.Record) string {
	for _, r := range recs {
		if r.PlanTask != "" {
			return r.PlanTask
		}
	}
	return ""
}

// dirtyPaths is every uncommitted path under top.
func dirtyPaths(top string) []string {
	raw, err := proc.RunGit(context.Background(), top, "--no-optional-locks", "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range proc.StatusEntries(raw) {
		out = append(out, e.Path)
	}
	return out
}

// dirtyOwned is the dirty paths that one of owned names or lies under.
func dirtyOwned(dirty, owned []string) []string {
	var out []string
	for _, p := range dirty {
		if slices.ContainsFunc(owned, func(o string) bool { return p == o || strings.HasPrefix(p, strings.TrimSuffix(o, "/")+"/") }) {
			out = append(out, p)
		}
	}
	return out
}

// withVerify puts the verify lines on top of the hand-back message, over any receipt the gate attached.
func withVerify(in *core.Input, res core.Result, lines []string) core.Result {
	if len(lines) == 0 || in.ToolName != "SubagentHandback" {
		return res
	}
	updated := res.UpdatedInput
	if updated == nil {
		if updated = in.CloneToolInput(); updated == nil {
			return res
		}
	}
	msg, _ := updated["message"].(string)
	updated["message"] = strings.Join(lines, "\n") + "\n\n" + msg
	res.UpdatedInput = updated
	return res
}
