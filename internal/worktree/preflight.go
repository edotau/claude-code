package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// Issue codes Preflight (and promote's recheck) can append to a MergePlan; each names one condition
// that must clear before Merge proceeds. IssueConflict is the one agent resolution acts on.
const (
	issueDetachedTarget     = "detached-target"
	issueTargetMismatch     = "target-mismatch"
	issueDirtyTarget        = "dirty-target"
	issueDirtyWorktree      = "dirty-worktree"
	issueInProgress         = "operation-in-progress"
	issueUnrelatedHistories = "unrelated-histories"
	issueCapability         = "merge-tree-capability"
	IssueConflict           = "conflict"
)

// Issue is one blocking condition Preflight or promote found, named by Code; Paths names the
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
	CommonDir      string
	MainGitDir     string // main checkout's own git-dir, resolved once by Preflight ("" = look it up)
	WorktreeGitDir string // the worktree's own git-dir, resolved once by Preflight ("" = look it up)
	WorktreePath   string
	TargetRef      string
	TargetOID      string
	SourceOID      string
	IntegratedOID  string // set by integrateTarget; promote refuses unless the worktree HEAD still matches
	// AfterResolution: planned after an agent resolution, so untracked tool residue in the worktree is tolerated.
	AfterResolution bool
	Issues          []Issue
}

// gitEnv returns os.Environ() with per-repo GIT_* overrides stripped, plus extra.
func gitEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_PREFIX":
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// runGit is the one seam over external git invocation in this file and merge.go: a test fakes a
// capability failure or an unsupported exit code through it, without a specially built git binary.
// extraEnv is appended to the subprocess environment; nil for the common case.
var runGit = func(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(extraEnv...)
	cmd.WaitDelay = gitWaitDelay // after cancel, stop waiting on inherited pipes instead of blocking
	return cmd.Output()
}

// gitWaitDelay bounds how long a cancelled git call may hold its output pipes open before being abandoned.
var gitWaitDelay = 5 * time.Second

// Preflight resolves name to its worktree path, rejects an unsafe starting state (steps 1), pins both
// OIDs and simulates the merge (step 2), and returns the plan either way — a non-nil error means
// Preflight itself could not run (bad name, missing worktree, git infra failure), never a rejected state.
// ctx cancellation stops the next git call; Preflight does no writes, so a cancel never leaves partial state.
func Preflight(ctx context.Context, repo *Repo, name, target string) (*MergePlan, error) {
	return preflight(ctx, repo, name, target, false)
}

// preflightAfterResolution re-plans once an agent resolution is committed: the first Preflight proved the worktree
// held no untracked files, so any now present are the agent's tooling residue (e.g. a plugin cache) — they cannot
// ride into the promoted commits, and Clean still keeps the worktree while they exist.
func preflightAfterResolution(ctx context.Context, repo *Repo, name, target string) (*MergePlan, error) {
	return preflight(ctx, repo, name, target, true)
}

func preflight(ctx context.Context, repo *Repo, name, target string, trackedOnlyWorktree bool) (*MergePlan, error) {
	plan, err := newMergePlan(ctx, repo, name, trackedOnlyWorktree)
	if err != nil {
		return nil, err
	}
	if stop, err := screenCheckouts(ctx, repo, plan, target, trackedOnlyWorktree); err != nil {
		return nil, err
	} else if stop {
		return plan, nil
	}
	if err := pinOIDs(ctx, repo, plan); err != nil {
		return nil, err
	}
	if err := simulateIssues(ctx, repo, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// newMergePlan resolves the worktree and fills the plan's paths and git dirs.
func newMergePlan(ctx context.Context, repo *Repo, name string, trackedOnlyWorktree bool) (*MergePlan, error) {
	worktreePath, err := resolveWorktree(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(worktreePath); err != nil {
		return nil, fmt.Errorf("worktree %q not found at %s", name, worktreePath)
	}
	commonDir := repo.CommonDir
	if commonDir == "" {
		if commonDir, err = commonDirOf(ctx, repo.Main); err != nil {
			return nil, err
		}
	}
	plan := &MergePlan{CommonDir: commonDir, WorktreePath: worktreePath, AfterResolution: trackedOnlyWorktree}
	if plan.MainGitDir, err = gitDirOf(ctx, repo.Main); err != nil {
		return nil, err
	}
	if plan.WorktreeGitDir, err = gitDirOf(ctx, worktreePath); err != nil {
		return nil, err
	}
	return plan, nil
}

// screenCheckouts records an issue for a detached or mismatched target and a dirty or mid-operation checkout; stop
// is true once the plan holds any issue.
func screenCheckouts(ctx context.Context, repo *Repo, plan *MergePlan, target string, trackedOnlyWorktree bool) (stop bool, err error) {
	if stop, err := screenTargetBranch(ctx, repo, plan, target); err != nil || stop {
		return stop, err
	}

	targetOID, err := headOfContext(ctx, repo.Main)
	if err != nil {
		return false, err
	}
	sourceOID, err := headOfContext(ctx, plan.WorktreePath)
	if err != nil {
		return false, err
	}
	if issue, err := checkTargetState(ctx, repo.Main, plan.MainGitDir, targetOID+"..."+sourceOID); err != nil {
		return false, err
	} else if issue != nil {
		plan.Issues = append(plan.Issues, *issue)
	}
	if issue, err := checkWorktreeState(ctx, plan.WorktreePath, plan.WorktreeGitDir, trackedOnlyWorktree); err != nil {
		return false, err
	} else if issue != nil {
		plan.Issues = append(plan.Issues, *issue)
	}
	return len(plan.Issues) > 0, nil // dirty or mid-operation: stop before pinning OIDs or writing anything
}

// screenTargetBranch records the main checkout's branch on plan; stop is true for a detached or non-target checkout.
func screenTargetBranch(ctx context.Context, repo *Repo, plan *MergePlan, target string) (stop bool, err error) {
	branch, err := requireBranch(ctx, repo.Main)
	if err != nil {
		if !errors.Is(err, errDetachedHead) {
			return false, err
		}
		plan.Issues = append(plan.Issues, Issue{Code: issueDetachedTarget, Paths: []string{repo.Main}, Detail: err.Error()})
		return true, nil
	}
	plan.TargetRef = branch
	if target != "" && target != branch {
		plan.Issues = append(plan.Issues, Issue{Code: issueTargetMismatch,
			Detail: fmt.Sprintf("main checkout is on %q, requested target was %q", branch, target)})
		return true, nil
	}
	return false, nil
}

// pinOIDs fixes the target and source HEADs on the plan.
func pinOIDs(ctx context.Context, repo *Repo, plan *MergePlan) (err error) {
	plan.TargetOID, err = headOfContext(ctx, repo.Main)
	if err != nil {
		return err
	}
	plan.SourceOID, err = headOfContext(ctx, plan.WorktreePath)
	if err != nil {
		return err
	}
	if plan.TargetOID == "" || plan.SourceOID == "" {
		return fmt.Errorf("could not resolve HEAD for %s or %s", repo.Main, plan.WorktreePath)
	}
	return nil
}

// simulateIssues adds the unrelated-history, capability or conflict issue the dry-run merge predicts.
func simulateIssues(ctx context.Context, repo *Repo, plan *MergePlan) error {
	unrelated, err := unrelatedHistories(ctx, repo.Main, plan.SourceOID, plan.TargetOID)
	if err != nil {
		return err
	}
	if unrelated {
		plan.Issues = append(plan.Issues, Issue{Code: issueUnrelatedHistories,
			Detail: fmt.Sprintf("%s and %s share no common ancestor", plan.SourceOID, plan.TargetOID)})
		return nil
	}

	conflicts, err := simulateMerge(ctx, repo.Main, plan.CommonDir, plan.SourceOID, plan.TargetOID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		plan.Issues = append(plan.Issues, Issue{Code: issueCapability, Detail: err.Error()})
		return nil
	}
	if len(conflicts) > 0 {
		plan.Issues = append(plan.Issues, Issue{Code: IssueConflict, Paths: conflicts,
			Detail: "git merge-tree predicts a conflict; merge --resolve-agent can resolve it"})
	}
	return nil
}

// simulateMerge runs `git merge-tree --write-tree --name-only -z` for source into target and reports
// the conflicted paths (nil = clean). A non-nil error means the simulation itself could not be
// trusted — a capability failure or an exit code outside {0,1} — and is never a signal to proceed as
// if it were clean. It isolates every object the write side effect produces into a throwaway
// alternate object directory, removed before return, so repoPath's real store is never touched.
// commonDir is repoPath's common dir when already known ("" = look it up).
func simulateMerge(ctx context.Context, repoPath, commonDir, sourceOID, targetOID string) (conflicts []string, err error) {
	args := []string{"merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", sourceOID, targetOID}
	env, cleanup, isoErr := isolatedObjectDir(ctx, repoPath, commonDir)
	if isoErr != nil {
		return nil, fmt.Errorf("merge-tree dry-run isolation: %w", isoErr)
	}
	defer func() { err = errors.Join(err, cleanup()) }()
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
func isolatedObjectDir(ctx context.Context, repoPath, commonDir string) ([]string, func() error, error) {
	if commonDir == "" {
		var err error
		if commonDir, err = commonDirOf(ctx, repoPath); err != nil {
			return nil, nil, err
		}
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

// checkTargetState blocks local target edits only where the promoted range touches the same paths.
func checkTargetState(ctx context.Context, path, gitDir, commitRange string) (*Issue, error) {
	dirty, err := targetDirtyPaths(ctx, path)
	if err != nil {
		return nil, err
	}
	if len(dirty) > 0 {
		touched, err := targetTouchedPaths(ctx, path, commitRange)
		if err != nil {
			return nil, err
		}
		var overlap []string
		for _, name := range touched {
			if dirty[name] {
				overlap = append(overlap, name)
			}
		}
		if len(overlap) > 0 {
			return &Issue{Code: issueDirtyTarget, Paths: overlap,
				Detail: "uncommitted target changes overlap promoted files: " + withHolders(path, overlap)}, nil
		}
	}
	return inProgressIssue(ctx, path, gitDir)
}

// withHolders lists rels, each followed by the live sessions or leaves the claims ledger says are editing it.
func withHolders(top string, rels []string) string {
	root := paths.CanonDeep(top)
	abs := make([]string, len(rels))
	for i, r := range rels {
		abs[i] = filepath.Join(root, r)
	}
	held, err := holders(proc.Alive, abs)
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = r
		if who := held[abs[i]]; len(who) > 0 {
			slices.Sort(who)
			out[i] += " (held by " + strings.Join(who, ", ") + ")"
		}
	}
	if err != nil {
		return strings.Join(out, ", ") + " (claims ledger partly unreadable, holders may be missing: " + err.Error() + ")"
	}
	return strings.Join(out, ", ")
}

// targetDirtyPaths reads NUL-delimited status, including both names of a rename or copy.
func targetDirtyPaths(ctx context.Context, path string) (map[string]bool, error) {
	out, err := runGit(ctx, path, nil, "status", "--porcelain=v1", "-z", "--ignore-submodules=none", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("git status in %s: %w", path, err)
	}
	dirty := make(map[string]bool)
	for _, e := range proc.StatusEntries(string(out)) {
		dirty[e.Path] = true
		if e.From != "" {
			dirty[e.From] = true
		}
	}
	return dirty, nil
}

// targetTouchedPaths disables rename folding so both the deleted and added paths participate.
func targetTouchedPaths(ctx context.Context, path, commitRange string) ([]string, error) {
	out, err := runGit(ctx, path, nil, "diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z", "--no-renames", commitRange, "--")
	if err != nil {
		if target, source, threeDot := strings.Cut(commitRange, "..."); threeDot {
			unrelated, ancestryErr := unrelatedHistories(ctx, path, source, target)
			if ancestryErr != nil {
				return nil, ancestryErr
			}
			if unrelated {
				return nil, nil // Leave unrelated histories to the existing later preflight check.
			}
		}
		return nil, fmt.Errorf("git diff in %s: %w", path, err)
	}
	return strings.FieldsFunc(string(out), func(r rune) bool { return r == '\x00' }), nil
}

// inProgressIssue is the issueInProgress for a merge/rebase/cherry-pick underway in path, or nil.
func inProgressIssue(ctx context.Context, path, gitDir string) (*Issue, error) {
	op, found, err := inProgressOp(ctx, path, gitDir)
	if err != nil || !found {
		return nil, err
	}
	return &Issue{Code: issueInProgress, Paths: []string{path}, Detail: op + " in progress"}, nil
}

// checkCheckoutState folds the dirty and in-progress-operation checks for one checkout into a single
// blocking Issue (dirty wins when both are true; the operation marker is secondary context).
func checkCheckoutState(ctx context.Context, path, gitDir string, dirtyProbe func(context.Context, string) (bool, error)) (*Issue, error) {
	dirty, err := dirtyProbe(ctx, path)
	if err != nil {
		return nil, err
	}
	if dirty {
		return &Issue{Code: issueDirtyWorktree, Paths: []string{path}}, nil
	}
	return inProgressIssue(ctx, path, gitDir)
}

// trackedDirty reports tracked changes only, ignoring untracked residue.
func trackedDirty(ctx context.Context, path string) (bool, error) {
	tracked, err := changedPaths(ctx, path)
	return len(tracked) > 0, err
}

// checkWorktreeState is checkCheckoutState for the source worktree; trackedOnly ignores untracked residue.
func checkWorktreeState(ctx context.Context, path, gitDir string, trackedOnly bool) (*Issue, error) {
	probe := isDirty
	if trackedOnly {
		probe = trackedDirty
	}
	return checkCheckoutState(ctx, path, gitDir, probe)
}

// isDirty reports uncommitted changes in path (Preflight and inspect share it). --untracked-files=all
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

// markerExists stats gitDir/name; only a confirmed os.IsNotExist is "absent", any other stat error propagates.
func markerExists(gitDir, name string) (bool, error) {
	_, err := os.Stat(filepath.Join(gitDir, name))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

// inProgressOp reports the mid-operation marker under path's own git-dir ("" = look it up), if any. Only a
// confirmed os.IsNotExist counts as "no marker"; any other stat error is an unknown state and propagates.
func inProgressOp(ctx context.Context, path, gitDir string) (op string, found bool, err error) {
	if gitDir == "" {
		if gitDir, err = gitDirOf(ctx, path); err != nil {
			return "", false, err
		}
	}
	for _, m := range opMarkers {
		exists, statErr := markerExists(gitDir, m.marker)
		if statErr != nil {
			return "", false, fmt.Errorf("check %s in %s: %w", m.marker, gitDir, statErr)
		}
		if exists {
			return m.op, true, nil
		}
	}
	return "", false, nil
}

// requireBranch is currentBranch's stricter sibling (list.go): Preflight must reject a detached
// target outright rather than silently keying off its OID the way the staleness check tolerates.
func requireBranch(ctx context.Context, path string) (string, error) {
	out, err := runGit(ctx, path, nil, "symbolic-ref", "--short", "-q", "HEAD")
	if code, ok := asExitErr(err); ok && code == 1 {
		return "", fmt.Errorf("%s: %w", path, errDetachedHead)
	}
	if err != nil {
		return "", fmt.Errorf("symbolic-ref in %s: %w", path, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// errDetachedHead is requireBranch's only "not on a branch" verdict; every other git failure is a real error.
var errDetachedHead = errors.New("HEAD is detached, not on a branch")

// unrelatedHistories reports whether source and target share no common ancestor, using asExitErr's
// contract (exit 1 = clean answer, anything else = a failure).
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
