package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Issue codes Preflight (and Promote's recheck) can append to a MergePlan; each names one condition
// that must clear before Merge proceeds. IssueConflict is the one Phase 3 (agent resolution) acts on.
const (
	IssueDetachedTarget     = "detached-target"
	IssueTargetMismatch     = "target-mismatch"
	IssueDirtyTarget        = "dirty-target"
	IssueDirtyWorktree      = "dirty-worktree"
	IssueInProgress         = "operation-in-progress"
	IssueUnrelatedHistories = "unrelated-histories"
	IssueCapability         = "merge-tree-capability"
	IssueConflict           = "conflict"
)

// Issue is one blocking condition Preflight or Promote found, named by Code; Paths names the
// checkout(s) or conflicted files involved, Detail carries a human-readable reason.
type Issue struct {
	Code   string
	Paths  []string
	Detail string
}

// MergePlan is Preflight's pinned, read-only snapshot of a worktree merge: both OIDs, the resolved
// target branch, and every Issue found. A non-empty Issues means Merge must not proceed — the plan
// itself is still returned so a caller (notably `worktree merge --dry-run`) can show why.
type MergePlan struct {
	CommonDir     string
	MainPath      string
	WorktreePath  string
	TargetRef     string
	TargetOID     string
	SourceOID     string
	IntegratedOID string // set by IntegrateTarget; Promote refuses unless the worktree HEAD still matches
	// AfterResolution: planned after an agent resolution, so untracked tool residue in the worktree is tolerated.
	AfterResolution bool
	Issues          []Issue
}

// runGit is the one seam over external git invocation in this file and merge.go: a test fakes a
// capability failure or an unsupported exit code through it, without a specially built git binary.
// extraEnv is appended to the subprocess environment; nil for the common case.
var runGit = func(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	return cmd.Output()
}

// Preflight resolves name to its worktree path, rejects an unsafe starting state (steps 1), pins both
// OIDs and simulates the merge (step 2), and returns the plan either way — a non-nil error means
// Preflight itself could not run (bad name, missing worktree, git infra failure), never a rejected state.
// ctx cancellation stops the next git call; Preflight does no writes, so a cancel never leaves partial state.
func Preflight(ctx context.Context, repo *Repo, name, target string) (*MergePlan, error) {
	return preflight(ctx, repo, name, target, false)
}

// PreflightAfterResolution re-plans once an agent resolution is committed: the first Preflight proved the worktree
// held no untracked files, so any now present are the agent's tooling residue (e.g. a plugin cache) — they cannot
// ride into the promoted commits, and Clean still keeps the worktree while they exist.
func PreflightAfterResolution(ctx context.Context, repo *Repo, name, target string) (*MergePlan, error) {
	return preflight(ctx, repo, name, target, true)
}

func preflight(ctx context.Context, repo *Repo, name, target string, trackedOnlyWorktree bool) (*MergePlan, error) {
	worktreePath, err := resolveWorktree(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(worktreePath); err != nil {
		return nil, fmt.Errorf("worktree %q not found at %s", name, worktreePath)
	}
	commonDir, err := commonDirOf(ctx, repo.Main)
	if err != nil {
		return nil, err
	}
	plan := &MergePlan{CommonDir: commonDir, MainPath: repo.Main, WorktreePath: worktreePath, AfterResolution: trackedOnlyWorktree}

	branch, err := requireBranch(ctx, repo.Main)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		plan.Issues = append(plan.Issues, Issue{Code: IssueDetachedTarget, Paths: []string{repo.Main}, Detail: err.Error()})
		return plan, nil
	}
	plan.TargetRef = branch
	if target != "" && target != branch {
		plan.Issues = append(plan.Issues, Issue{Code: IssueTargetMismatch,
			Detail: fmt.Sprintf("main checkout is on %q, requested target was %q", branch, target)})
		return plan, nil
	}

	if issue, err := checkCheckoutState(ctx, repo.Main, IssueDirtyTarget); err != nil {
		return nil, err
	} else if issue != nil {
		plan.Issues = append(plan.Issues, *issue)
	}
	if issue, err := checkWorktreeState(ctx, worktreePath, trackedOnlyWorktree); err != nil {
		return nil, err
	} else if issue != nil {
		plan.Issues = append(plan.Issues, *issue)
	}
	if len(plan.Issues) > 0 {
		return plan, nil // dirty or mid-operation: stop before pinning OIDs or writing anything
	}

	plan.TargetOID, err = headOfContext(ctx, repo.Main)
	if err != nil {
		return nil, err
	}
	plan.SourceOID, err = headOfContext(ctx, worktreePath)
	if err != nil {
		return nil, err
	}
	if plan.TargetOID == "" || plan.SourceOID == "" {
		return nil, fmt.Errorf("could not resolve HEAD for %s or %s", repo.Main, worktreePath)
	}

	unrelated, err := unrelatedHistories(ctx, repo.Main, plan.SourceOID, plan.TargetOID)
	if err != nil {
		return nil, err
	}
	if unrelated {
		plan.Issues = append(plan.Issues, Issue{Code: IssueUnrelatedHistories,
			Detail: fmt.Sprintf("%s and %s share no common ancestor", plan.SourceOID, plan.TargetOID)})
		return plan, nil
	}

	conflicts, err := SimulateMerge(ctx, repo.Main, plan.SourceOID, plan.TargetOID, true)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		plan.Issues = append(plan.Issues, Issue{Code: IssueCapability, Detail: err.Error()})
		return plan, nil
	}
	if len(conflicts) > 0 {
		plan.Issues = append(plan.Issues, Issue{Code: IssueConflict, Paths: conflicts,
			Detail: "git merge-tree predicts a conflict; Phase 3 adds agent resolution for this"})
	}
	return plan, nil
}

// SimulateMerge runs `git merge-tree --write-tree --name-only -z` for source into target and reports
// the conflicted paths (nil = clean). A non-nil error means the simulation itself could not be
// trusted — a capability failure or an exit code outside {0,1} — and is never a signal to proceed as
// if it were clean. dryRun isolates every object the write side effect produces into a throwaway
// alternate object directory, removed before return, so repoPath's real store is never touched.
func SimulateMerge(ctx context.Context, repoPath, sourceOID, targetOID string, dryRun bool) (conflicts []string, err error) {
	args := []string{"merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", sourceOID, targetOID}
	var env []string
	if dryRun {
		extraEnv, cleanup, err := isolatedObjectDir(ctx, repoPath)
		if err != nil {
			return nil, fmt.Errorf("merge-tree dry-run isolation: %w", err)
		}
		defer func() { err = errors.Join(err, cleanup()) }()
		env = extraEnv
	}
	out, err := runGit(ctx, repoPath, env, args...)
	switch code, isExit := asExitErr(err); {
	case err == nil:
		return nil, nil // exit 0: clean, nothing past the written tree OID to parse
	case isExit && code == 1:
		return parseConflicts(out), nil
	default:
		return nil, fmt.Errorf("git merge-tree: %w (%s)", err, gitErrorDetail(err))
	}
}

// isolatedObjectDir makes a scratch object directory with repoPath's real one layered as an alternate
// (read access to every existing object, writes landing only in the scratch dir — see
// git-merge-tree(1) and the GIT_OBJECT_DIRECTORY/GIT_ALTERNATE_OBJECT_DIRECTORIES env vars it honors).
func isolatedObjectDir(ctx context.Context, repoPath string) ([]string, func() error, error) {
	commonDir, err := commonDirOf(ctx, repoPath)
	if err != nil {
		return nil, nil, err
	}
	tmp, err := os.MkdirTemp("", "worktree-merge-tree-*")
	if err != nil {
		return nil, nil, err
	}
	objDir := filepath.Join(tmp, "objects")
	if err := os.MkdirAll(objDir, 0o700); err != nil {
		return nil, nil, errors.Join(err, os.RemoveAll(tmp))
	}
	env := []string{
		"GIT_OBJECT_DIRECTORY=" + objDir,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(commonDir, "objects"),
	}
	return env, func() error {
		if err := os.RemoveAll(tmp); err != nil {
			return fmt.Errorf("remove merge-tree scratch directory: %w", err)
		}
		return nil
	}, nil
}

// parseConflicts reads merge-tree's -z/--name-only/--no-messages output: the written tree OID as the
// first NUL-terminated field, then one conflicted path per remaining non-empty field. It never greps
// for conflict markers — the structured field layout is the only thing trusted here.
func parseConflicts(out []byte) []string {
	fields := strings.Split(string(out), "\x00")
	var conflicts []string
	for _, f := range fields[1:] {
		if f != "" {
			conflicts = append(conflicts, f)
		}
	}
	return conflicts
}

// checkCheckoutState folds the dirty and in-progress-operation checks for one checkout into a single
// blocking Issue (dirty wins when both are true; the operation marker is secondary context).
func checkCheckoutState(ctx context.Context, path, dirtyCode string) (*Issue, error) {
	dirty, err := isDirty(ctx, path)
	if err != nil {
		return nil, err
	}
	if dirty {
		return &Issue{Code: dirtyCode, Paths: []string{path}}, nil
	}
	op, found, err := inProgressOp(ctx, path)
	if err != nil {
		return nil, err
	}
	if found {
		return &Issue{Code: IssueInProgress, Paths: []string{path}, Detail: op + " in progress"}, nil
	}
	return nil, nil
}

// checkWorktreeState is checkCheckoutState for the source worktree; trackedOnly ignores untracked residue.
func checkWorktreeState(ctx context.Context, path string, trackedOnly bool) (*Issue, error) {
	if !trackedOnly {
		return checkCheckoutState(ctx, path, IssueDirtyWorktree)
	}
	tracked, err := changedPaths(ctx, path)
	if err != nil {
		return nil, err
	}
	if len(tracked) > 0 {
		return &Issue{Code: IssueDirtyWorktree, Paths: []string{path}}, nil
	}
	op, found, err := inProgressOp(ctx, path)
	if err != nil {
		return nil, err
	}
	if found {
		return &Issue{Code: IssueInProgress, Paths: []string{path}, Detail: op + " in progress"}, nil
	}
	return nil, nil
}

// isDirty mirrors Inspect's own check in list.go; kept local since Preflight checks the main checkout
// as well as a worktree, and Inspect's signature only ever runs against the latter. --untracked-files=all
// overrides a repo-local status.showUntrackedFiles=no, which would otherwise hide new files from this check.
func isDirty(ctx context.Context, path string) (bool, error) {
	out, err := runGit(ctx, path, nil, "status", "--porcelain", "--ignore-submodules=none", "--untracked-files=all")
	if err != nil {
		return false, fmt.Errorf("git status in %s: %w", path, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// opMarkers names the per-worktree git-dir markers for a mid-operation state; a linked worktree's own
// git-dir (never --git-common-dir) carries these, distinct per worktree. MERGE_AUTOSTASH means a
// restore of the pre-merge local edits is still pending — see lifecycle.go's Clean, which refuses on it.
var opMarkers = []struct{ marker, op string }{
	{"MERGE_HEAD", "merge"},
	{"MERGE_AUTOSTASH", "merge (autostash restore pending)"},
	{"CHERRY_PICK_HEAD", "cherry-pick"},
	{"rebase-merge", "rebase"},
	{"rebase-apply", "rebase"},
}

// inProgressOp reports the name of a mid-operation marker found under path's own git-dir, if any. Only
// a confirmed os.IsNotExist counts as "no marker" — any other stat error (e.g. permission denied) is an
// unknown state and must propagate, never be read as clean.
func inProgressOp(ctx context.Context, path string) (op string, found bool, err error) {
	gitDir, err := gitDirOf(ctx, path)
	if err != nil {
		return "", false, err
	}
	for _, m := range opMarkers {
		_, statErr := os.Stat(filepath.Join(gitDir, m.marker))
		if statErr == nil {
			return m.op, true, nil
		}
		if !os.IsNotExist(statErr) {
			return "", false, fmt.Errorf("check %s in %s: %w", m.marker, gitDir, statErr)
		}
	}
	return "", false, nil
}

// requireBranch is currentBranch's stricter sibling (list.go): Preflight must reject a detached
// target outright rather than silently keying off its OID the way the staleness check tolerates.
func requireBranch(ctx context.Context, path string) (string, error) {
	out, err := runGit(ctx, path, nil, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return "", fmt.Errorf("%s: HEAD is detached, not on a branch", path)
	}
	return strings.TrimSpace(string(out)), nil
}

// unrelatedHistories reports whether source and target share no common ancestor, using the same
// exit-code contract list.go's Inspect already relies on (1 = clean "no", anything else = a failure).
func unrelatedHistories(ctx context.Context, repoPath, sourceOID, targetOID string) (bool, error) {
	_, err := runGit(ctx, repoPath, nil, "merge-base", sourceOID, targetOID)
	if err == nil {
		return false, nil
	}
	if code, ok := asExitErr(err); ok && code == 1 {
		return true, nil
	}
	return false, err
}

// isAncestor reports whether ancestor is reachable from descendant (fast-forward is possible),
// via the same exit-code contract as unrelatedHistories.
func isAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	_, err := runGit(ctx, repoPath, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if code, ok := asExitErr(err); ok && code == 1 {
		return false, nil
	}
	return false, err
}

func commonDirOf(ctx context.Context, path string) (string, error) {
	out, err := runGit(ctx, path, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("git-common-dir in %s: %w", path, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitDirOf(ctx context.Context, path string) (string, error) {
	out, err := runGit(ctx, path, nil, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("git-dir in %s: %w", path, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitErrorDetail recovers a failed command's own stderr (exec.Cmd.Output populates it on an
// *exec.ExitError) so a blocked merge reports git's reason, not just "exit status N".
func gitErrorDetail(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}
