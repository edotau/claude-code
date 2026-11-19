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
	task, ok := docs.BriefTask(in.CWD, lr.scan.brief, recordPlanTask(loadRecords(lr.ids)))
	if !ok {
		return nil
	}
	id := cmp.Or(task.ID, task.Title)
	if time.Until(deadline) < 2*time.Second {
		return []string{"verify " + id + ": no time left in the hand-back budget; the commit-time verify still runs"}
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	top := memory.GitToplevel(in.CWD)
	checks := waves.VerifyTask(ctx, waves.TaskVerifyInput{Dir: top, Task: task, Files: dirtyOwned(ctx, top, plans.OwnedPaths(task)),
		OutDir: filepath.Join(paths.StateDir(), "verify", "handback")})
	lines := make([]string, 0, len(checks))
	for _, c := range checks {
		lines = append(lines, c.Line())
	}
	return lines
}

func recordPlanTask(recs []agent.Record) string {
	for _, r := range recs {
		if r.PlanTask != "" {
			return r.PlanTask
		}
	}
	return ""
}

// dirtyOwned is the uncommitted paths under top that one of owned names or lies under.
func dirtyOwned(ctx context.Context, top string, owned []string) []string {
	raw, err := proc.RunGit(ctx, top, "--no-optional-locks", "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range proc.StatusEntries(raw) {
		if slices.ContainsFunc(owned, func(o string) bool { return e.Path == o || strings.HasPrefix(e.Path, strings.TrimSuffix(o, "/")+"/") }) {
			out = append(out, e.Path)
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
