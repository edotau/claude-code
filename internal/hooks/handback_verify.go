package hooks

import (
	"cmp"
	"context"
	"os"
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
	top := verifyTop(in.CWD, lr.scan.brief)
	if top == "" {
		return nil
	}
	lane := leafCheckout(in, top)
	dirtyTop := cmp.Or(lane, top)
	dirtyc := make(chan []string, 1)
	go func() { dirtyc <- dirtyPaths(dirtyTop) }() // git status overlaps the plan parse
	planPath, active, hasActive := plans.Active(plans.Home(top), plans.WorktreeName(dirtyTop), plans.MirrorDests())
	dirty := <-dirtyc
	recTask := recordPlanTask(loadRecords(lr.ids))
	named, task, isNamed, ok := docs.NamedPlanTask(lr.scan.brief, recTask)
	if isNamed {
		if !ok {
			return nil
		}
		planPath = named
	} else if task, ok = docs.PlanBriefTask(active, lr.scan.brief, recTask); !hasActive || !ok {
		if task, ok = untiedTask(active, dirty); !ok {
			return nil
		}
	}
	id := cmp.Or(task.ID, task.Title)
	files := dirtyOwned(dirty, plans.OwnedPaths(task))
	commit := commitLine(lane, id, planPath, files)
	if lane == "" && len(files) == 0 { // no lane and no owned edit in the checkout: nothing shows where the leaf worked
		return []string{"verify " + id + ": lane not resolved; not run", commit}
	}
	if time.Until(deadline) < 2*time.Second {
		return []string{"verify " + id + ": no time left in the hand-back budget; the commit-time verify still runs", commit}
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	checks := waves.VerifyTask(ctx, waves.TaskVerifyInput{Dir: dirtyTop, Task: task, Files: files,
		OutDir: filepath.Join(paths.StateDir(), "verify", "handback")})
	return append(verifyLines(checks), commit)
}

// commitLine is the commit command, run from the lane when the leaf worked in one.
func commitLine(lane, id, planPath string, files []string) string {
	cd := ""
	if lane != "" {
		cd = "cd '" + lane + "' && "
	}
	return "commit+tick: " + cd + "claude-code worktree commit --task '" + id + "' --plan " + planPath +
		` -m "<type>: <subject>"` + strings.Join(append([]string{""}, files...), " ")
}

// leafCheckout is the lane the leaf worked in when it belongs to top's repo; "" when none resolves.
func leafCheckout(in *core.Input, top string) string {
	lane := leafLane(withAgentID(in))
	if lane == "" || !paths.SamePath(paths.MainCheckout(lane), paths.MainCheckout(top)) {
		return ""
	}
	return lane
}

// withAgentID is in with the hand-back's subagent id, which leafLane reads from AgentID.
func withAgentID(in *core.Input) *core.Input {
	c := *in
	c.AgentID = in.SubagentID()
	return &c
}

// verifyLines is one line per finished check, then one line for the deadline cut; "no checks ran" is dropped after it.
func verifyLines(checks []waves.TaskCheck) []string {
	cut := slices.IndexFunc(checks, func(c waves.TaskCheck) bool { return strings.HasPrefix(c.Skip, "not finished") })
	if cut >= 0 {
		checks = checks[:cut+1]
	}
	lines := make([]string, 0, len(checks))
	for _, c := range checks {
		lines = append(lines, c.Line())
	}
	return lines
}

// checkoutTop is dir's git toplevel by stat walk; git runs only for a layout the walk cannot see (bare gitdir).
func checkoutTop(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real // git resolves cwd before its walk, so a lexical walk could find another repo
	}
	if top, _ := paths.CheckoutOf(dir); top != "" {
		return paths.Real(top) // git prints the symlink-resolved toplevel
	}
	return memory.GitToplevel(dir)
}

// verifyTop is cwd's checkout, or the harness when cwd is not the harness and the brief names harness paths.
func verifyTop(cwd, brief string) string {
	top := checkoutTop(cwd)
	cfg := paths.ConfigDir()
	if top != "" && (paths.SamePath(top, cfg) || paths.SamePath(paths.MainCheckout(top), cfg)) {
		return top // the harness itself or one of its linked worktrees
	}
	if htop := checkoutTop(cfg); htop != "" && namesHarness(brief, cfg, plans.Name(htop)) {
		return htop
	}
	return top
}

// namesHarness reports whether brief names a harness file or plan under cfg (absolute or ~/); other repos' plans skip.
func namesHarness(brief, cfg, planDir string) bool {
	forms := []string{filepath.ToSlash(cfg) + "/"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, ok := paths.RelWithin(home, cfg); ok && rel != "." {
			forms = append(forms, "~/"+filepath.ToSlash(rel)+"/")
		}
	}
	for _, f := range forms {
		for _, rest := range strings.Split(brief, f)[1:] {
			if !strings.HasPrefix(rest, "plans/") || strings.HasPrefix(rest, "plans/"+planDir+"/") {
				return true
			}
		}
	}
	return false
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
