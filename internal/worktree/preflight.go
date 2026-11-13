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

// Issue codes Preflight (or promote) can add to a MergePlan; IssueConflict is what a resolve agent acts on.
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

// Issue is one blocking condition named by Code; Paths names the checkouts or files, Detail the reason.
type Issue struct {
	Code   string
	Paths  []string
	Detail string
}

// MergePlan is Preflight's pinned, read-only snapshot; a non-empty Issues means Merge must not proceed.
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

// runGit is the one seam over git here and in merge.go, so tests fake failures; extraEnv is appended (nil = none).
var runGit = func(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := gitCmd(ctx, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd.Output()
}

// gitWaitDelay bounds how long a cancelled git call may hold its output pipes open before being abandoned.
var gitWaitDelay = 5 * time.Second

// Preflight resolves name, rejects unsafe state, pins both OIDs and simulates the merge; an error = it could not run.
func Preflight(ctx context.Context, repo *Repo, name, target string) (*MergePlan, error) {
	return preflight(ctx, repo, name, target, false)
}

// preflightAfterResolution re-plans after a committed agent resolution, tolerating untracked tooling residue.
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
	mainGitDir, commonDir, err := gitDirsOf(ctx, repo.Main)
	if err != nil {
		return nil, err
	}
	if repo.CommonDir != "" {
		commonDir = repo.CommonDir
	}
	plan := &MergePlan{CommonDir: commonDir, MainGitDir: mainGitDir, WorktreePath: worktreePath,
		AfterResolution: trackedOnlyWorktree}
	if plan.WorktreeGitDir, _, err = gitDirsOf(ctx, worktreePath); err != nil {
		return nil, err
	}
	return plan, nil
}

// screenCheckouts records an issue for a bad target or dirty/mid-operation checkout; with none, the plan pins HEADs.
func screenCheckouts(ctx context.Context, repo *Repo, plan *MergePlan, target string, trackedOnlyWorktree bool) (stop bool, err error) {
	if stop, err := screenTargetBranch(ctx, repo, plan, target); err != nil || stop {
		return stop, err
	}
	if plan.TargetOID, err = headOfContext(ctx, repo.Main); err != nil {
		return false, err
	}
	if plan.SourceOID, err = headOfContext(ctx, plan.WorktreePath); err != nil {
		return false, err
	}
	if err := screenStates(ctx, repo, plan, trackedOnlyWorktree); err != nil {
		return false, err
	}
	if len(plan.Issues) > 0 {
		plan.TargetOID, plan.SourceOID = "", "" // dirty or mid-operation: nothing is pinned
		return true, nil
	}
	return false, nil
}

// screenStates records an issue for a dirty or mid-operation target and worktree checkout.
func screenStates(ctx context.Context, repo *Repo, plan *MergePlan, trackedOnlyWorktree bool) error {
	if issue, err := checkTargetState(ctx, repo.Main, plan.MainGitDir, plan.TargetOID+"..."+plan.SourceOID); err != nil {
		return err
	} else if issue != nil {
		plan.Issues = append(plan.Issues, *issue)
	}
	if issue, err := checkWorktreeState(ctx, plan.WorktreePath, plan.WorktreeGitDir, trackedOnlyWorktree); err != nil {
		return err
	} else if issue != nil {
		plan.Issues = append(plan.Issues, *issue)
	}
	return nil
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

// simulateMerge runs `git merge-tree --write-tree` in a scratch object dir; nil = clean, an error is never clean.
func simulateMerge(ctx context.Context, repoPath, commonDir, sourceOID, targetOID string) (conflicts []string, err error) {
	args := []string{"merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", sourceOID, targetOID}
	env, cleanup, isoErr := isolatedObjectDir(ctx, repoPath, commonDir)
	if isoErr != nil {
		return nil, fmt.Errorf("merge-tree dry-run isolation: %w", isoErr)
	}
	defer func() { // a scratch-dir leak is reported only alongside a failure, never in place of a conflict verdict
		if cleanErr := cleanup(); err != nil {
			err = errors.Join(err, cleanErr)
		}
	}()
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

// isolatedObjectDir makes a scratch object dir with the real one as an alternate, so repoPath's store is never written.
func isolatedObjectDir(ctx context.Context, repoPath, commonDir string) ([]string, func() error, error) {
	if commonDir == "" {
		var err error
		if _, commonDir, err = gitDirsOf(ctx, repoPath); err != nil {
			return nil, nil, err
		}
	}
	if strings.ContainsRune(commonDir, os.PathListSeparator) {
		return nil, nil, fmt.Errorf("git common dir %q contains %q and cannot be an alternate object directory", commonDir, os.PathListSeparator)
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

// parseConflicts reads merge-tree -z output: tree OID first, then one conflicted path per field; never greps markers.
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

// checkCheckoutState folds the dirty and in-progress checks for one checkout into one blocking Issue (dirty wins).
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

// isDirty reports uncommitted changes; --untracked-files=all overrides a repo-local showUntrackedFiles=no.
func isDirty(ctx context.Context, path string) (bool, error) {
	out, err := runGit(ctx, path, nil, "status", "--porcelain", "--ignore-submodules=none", "--untracked-files=all")
	if err != nil {
		return false, fmt.Errorf("git status in %s: %w", path, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// opMarkers names the per-worktree git-dir mid-operation markers; MERGE_AUTOSTASH means a pre-merge restore is pending.
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

// inProgressOp reports the mid-operation marker in gitDir ("" = look it up); only IsNotExist means none.
func inProgressOp(ctx context.Context, path, gitDir string) (op string, found bool, err error) {
	if gitDir == "" {
		if gitDir, _, err = gitDirsOf(ctx, path); err != nil {
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

// requireBranch is currentBranch's stricter sibling: a detached target is rejected, not keyed off its OID.
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

// gitYesNo runs git in repoPath: exit 0 is true, the clean answer exit 1 is false, anything else an error.
func gitYesNo(ctx context.Context, repoPath string, args ...string) (bool, error) {
	_, err := runGit(ctx, repoPath, nil, args...)
	if code, ok := asExitErr(err); ok && code == 1 {
		return false, nil
	}
	return err == nil, err
}

// unrelatedHistories reports whether source and target share no ancestor (merge-base exit 1 = none).
func unrelatedHistories(ctx context.Context, repoPath, sourceOID, targetOID string) (bool, error) {
	related, err := gitYesNo(ctx, repoPath, "merge-base", sourceOID, targetOID)
	return !related && err == nil, err
}

// isAncestor reports whether ancestor is reachable from descendant.
func isAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	return gitYesNo(ctx, repoPath, "merge-base", "--is-ancestor", ancestor, descendant)
}

// gitDirsOf is path's git dir and common dir from one rev-parse.
func gitDirsOf(ctx context.Context, path string) (gitDir, commonDir string, err error) {
	out, err := runGit(ctx, path, nil, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return "", "", fmt.Errorf("git dirs in %s: %w", path, err)
	}
	gitDir, commonDir, ok := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if !ok {
		return "", "", fmt.Errorf("git dirs in %s: unexpected rev-parse output %q", path, out)
	}
	return gitDir, commonDir, nil
}

// gitErrorDetail recovers a failed command's stderr from an *exec.ExitError, so a block reports git's reason.
func gitErrorDetail(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}
