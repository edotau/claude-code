package safety

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/worktree"
)

// rewriteTimeout bounds the guard's git and process calls; safety-scan's budget is 5s.
const rewriteTimeout = 2 * time.Second

var rebaseResumeFlags = sync.OnceValue(func() map[string]bool { return shell.GuardSet("rebase_resume_flags") })
var rebaseValueFlags = sync.OnceValue(func() map[string]bool { return shell.GuardSet("rebase_value_flags") })
var updateRefValueFlags = sync.OnceValue(func() map[string]bool { return shell.GuardSet("update_ref_value_flags") })
var branchValueFlags = sync.OnceValue(func() map[string]bool { return shell.GuardSet("branch_value_flags") })

// rewriteRisk is one history rewrite: its verb, how many commits it rewrites, and why the tree is shared.
type rewriteRisk struct {
	verb, branch string
	commits      int
	reasons      []string
	warn         string
}

// rewriteVerdict blocks a history rewrite in a shared checkout; an infra error allows it and returns a warning.
func rewriteVerdict(in *core.Input, cmd string) (d core.Decision, msg, detail, warn string) {
	r, risky := rewriteScan(in, cmd, in.CWD)
	if !risky {
		return core.Allow, "", "", r.warn
	}
	what := fmt.Sprintf("git %s rewrites %d commit(s) on %s in a shared checkout: %s", r.verb, r.commits, r.branch,
		strings.Join(r.reasons, "; "))
	hint := "Make a new commit instead; undo a commit with git revert. " +
		"To rewrite history, do it in your own linked worktree: claude-code worktree create <slug>."
	return core.Block, core.SafetyMessage(in, core.Block, what, "", hint), "shared history rewrite", ""
}

// rewriteScan finds the first shared-tree rewrite in cmd, following `cd`, substitutions and nested `bash -c`/`eval`.
func rewriteScan(in *core.Input, cmd, cwd string) (rewriteRisk, bool) {
	var found rewriteRisk
	var warn string
	risky := shell.EachGit(withGitEnv(dropRemoteHeredocs(cmd)), cwd, func(target, verb string, args []string) bool {
		rng, ok := rewriteRange(verb, args)
		if !ok || verb == "branch" && in.AgentID == "" { // the owner deletes or moves branches from the main session
			return false
		}
		r, hit := assessRewrite(in, target, verb, rng)
		warn = cmp.Or(warn, r.warn)
		found = r
		return hit
	})
	if risky {
		return found, true
	}
	return rewriteRisk{warn: warn}, false
}

// rewriteRange is the rev-list range a git invocation would rewrite; ok=false means it rewrites nothing.
func rewriteRange(verb string, args []string) ([]string, bool) {
	switch verb {
	case "commit":
		if slices.Contains(args, "--amend") {
			return []string{"--max-count=1", "HEAD"}, true
		}
	case "rebase":
		if slices.ContainsFunc(args, func(a string) bool { return rebaseResumeFlags()[a] }) {
			return nil, false // acts on a rebase whose start was already checked
		}
		return rebaseRange(args), true
	case "update-ref":
		return updateRefRange(args)
	case "branch":
		return branchRange(args)
	case "reset":
		if ops := rewriteOperands(args, nil); len(ops) > 0 && ops[0] != "" && !slices.Contains(args, "--") {
			return []string{ops[0] + "..HEAD"}, true
		}
	}
	return nil, false
}

// lostCommits is the rev-list range of commits only ref's tip holds (--exclude names the branch without refs/heads/).
func lostCommits(ref, newTip string) []string {
	not := []string{"--not"}
	if newTip != "" {
		not = append(not, newTip)
	}
	return append(append([]string{ref}, not...), "--exclude="+strings.TrimPrefix(ref, "refs/heads/"), "--branches", "--tags", "--remotes")
}

// updateRefRange is the commits update-ref strands on any branch ref, checked out or not.
func updateRefRange(args []string) ([]string, bool) {
	ops := rewriteOperands(args, updateRefValueFlags())
	if len(ops) == 0 || !strings.HasPrefix(ops[0], "refs/heads/") {
		return nil, false
	}
	if slices.Contains(args, "-d") || slices.Contains(args, "--delete") {
		return lostCommits(ops[0], ""), true
	}
	if len(ops) > 1 {
		return lostCommits(ops[0], ops[1]), true
	}
	return nil, false
}

// branchRange is the commits branch -f/-D/-M strands by moving, deleting or overwriting a branch.
func branchRange(args []string) ([]string, bool) {
	ops := rewriteOperands(args, branchValueFlags())
	if len(ops) == 0 || slices.Contains(args, "--") {
		return nil, false
	}
	switch {
	case slices.ContainsFunc(args, func(a string) bool { return shortFlag(a, "D") }):
		return lostCommits("refs/heads/"+ops[0], ""), true
	case slices.ContainsFunc(args, func(a string) bool { return shortFlag(a, "M") }):
		return lostCommits("refs/heads/"+ops[len(ops)-1], ""), true
	case slices.ContainsFunc(args, func(a string) bool { return a == "--force" || shortFlag(a, "f") }):
		start := "HEAD"
		if len(ops) > 1 {
			start = ops[1]
		}
		return lostCommits("refs/heads/"+ops[0], start), true
	}
	return nil, false
}

// shortFlag reports a short option cluster (`-fu`) holding the letter.
func shortFlag(a, letter string) bool {
	return len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.Contains(a[1:], letter)
}

// rebaseRange selects the explicit target branch, or HEAD when rebasing the current branch.
func rebaseRange(args []string) []string {
	operands := rewriteOperands(args, rebaseValueFlags())
	if slices.Contains(args, "--root") {
		if len(operands) > 0 {
			return []string{operands[0]}
		}
		return []string{"HEAD"}
	}
	base, branch := "@{upstream}", "HEAD"
	if len(operands) > 0 {
		base = operands[0]
	}
	if len(operands) > 1 {
		branch = operands[1]
	}
	return []string{base + ".." + branch}
}

// rewriteOperands collects non-option words before `--`, skipping option values.
func rewriteOperands(args []string, valueFlags map[string]bool) []string {
	var operands []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return operands
		case valueFlags[a]:
			i++
		case !strings.HasPrefix(a, "-"):
			operands = append(operands, a)
		}
	}
	return operands
}

// assessRewrite is risky when the target tree is shared and the range holds a commit; errors fail open with a warning.
func assessRewrite(in *core.Input, target, verb string, rng []string) (rewriteRisk, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), rewriteTimeout)
	defer cancel()
	top, linked, n, err := probeRewrite(ctx, target, rng)
	if err != nil {
		return rewriteRisk{warn: rewriteWarn(verb, err)}, false // fail open
	}
	if n == 0 {
		return rewriteRisk{}, false // not a repo, a non-commit operand or an empty range: git rewrites nothing
	}
	_, branch, nested := worktree.CheckoutPeersInfo(ctx, top)
	if verb == "rebase" {
		ref := rng[0]
		if _, tip, ok := strings.Cut(ref, ".."); ok {
			ref = tip
		}
		if ref != "HEAD" {
			branch = ref
		}
	}
	if ref, ok := strings.CutPrefix(rng[0], "refs/heads/"); ok { // branch and update-ref name the ref they strand
		branch = ref
	}
	reasons, err := shareReasons(ctx, in, top, linked, nested)
	r := rewriteRisk{verb: verb, branch: branch, commits: n, reasons: reasons}
	if err != nil {
		r.warn = rewriteWarn(verb, err)
	}
	return r, len(reasons) > 0
}

// rewriteWarn is the stderr line for a rewrite the guard could not check.
func rewriteWarn(verb string, err error) string {
	return fmt.Sprintf("safety: WARNING — could not check git %s for a shared-history rewrite (%v); allowed\n", verb, err)
}

// probeRewrite resolves target's toplevel, whether it is a linked worktree, and the rewrite's commit count.
func probeRewrite(ctx context.Context, target string, rng []string) (string, bool, int, error) {
	dirs, ok, err := proc.Git(ctx, target, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir",
		"--git-common-dir")
	if err == nil && ok && len(dirs) != 3 { // git before 2.31 echoes --path-format as a line
		err = fmt.Errorf("rev-parse printed %d lines, want 3", len(dirs))
	}
	if err != nil || !ok {
		return "", false, 0, err
	}
	top := paths.Real(dirs[0])
	n, err := rewriteCount(ctx, top, rng)
	return top, paths.Real(dirs[1]) != paths.Real(dirs[2]), n, err
}

// rewriteCount is how many commits rng holds in dir; 0 when its base is not a commit (a path, no upstream).
func rewriteCount(ctx context.Context, dir string, rng []string) (int, error) {
	if base, _, isRange := strings.Cut(rng[0], ".."); isRange {
		if _, ok, err := proc.Git(ctx, dir, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil || !ok {
			return 0, err
		}
	}
	out, ok, err := proc.Git(ctx, dir, append([]string{"rev-list", "--count"}, rng...)...)
	if err != nil || !ok {
		return 0, err
	}
	n, _ := strconv.Atoi(out[0])
	return n, nil
}

// shareReasons lists why top is shared: the live config tree, peer sessions in it, or this session's leaves.
func shareReasons(ctx context.Context, in *core.Input, top string, linked bool, nested []string) ([]string, error) {
	var reasons []string
	if top == paths.Real(paths.ConfigDir()) {
		reasons = append(reasons, "it is the live config tree "+top)
	}
	procs, err := worktree.PeersOf(ctx, top, nested)
	for _, p := range procs {
		reasons = append(reasons, worktree.SessionLabel(p)+" works in it")
	}
	if linked {
		return reasons, err
	}
	if in.AgentID != "" {
		// a leaf's temp repo, or any repo outside the config tree, is no main tree
		if tempRemovable(top) || !paths.InsideDir(top, paths.ConfigDir()) {
			return reasons, err
		}
		return append(reasons, "you are a leaf in the main tree"), err
	}
	if owner := inflight.SessionOwner(); owner != 0 && paths.InsideDir(top, paths.ConfigDir()) {
		if n := inflight.CountOwned(owner, time.Now()); n > 0 {
			reasons = append(reasons, fmt.Sprintf("this session has %d live leaf agent(s) in the main tree", n))
		}
	}
	return reasons, err
}
