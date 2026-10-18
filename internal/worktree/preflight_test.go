package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mergeFixtureWrite writes content to file in dir and commits it with msg; a shared helper for every
// Preflight/Merge test building up divergent history on top of gitRepo's single init commit.
func mergeFixtureWrite(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mergeFixtureRun(t, dir, "add", file)
	mergeFixtureRun(t, dir, "commit", "-q", "-m", msg)
}

// mergeFixtureRename renames oldName to newName and commits.
func mergeFixtureRename(t *testing.T, dir, oldName, newName, msg string) {
	t.Helper()
	mergeFixtureRun(t, dir, "mv", oldName, newName)
	mergeFixtureRun(t, dir, "commit", "-q", "-m", msg)
}

// mergeFixtureRemove deletes file and commits the deletion.
func mergeFixtureRemove(t *testing.T, dir, file, msg string) {
	t.Helper()
	mergeFixtureRun(t, dir, "rm", "-q", file)
	mergeFixtureRun(t, dir, "commit", "-q", "-m", msg)
}

func mergeFixtureRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// mergeFixtureSetup makes a fresh main repo with a base file commit, plus a worktree named "feature"
// branched from it — the shared starting point for every divergence scenario below.
func mergeFixtureSetup(t *testing.T) (repo *Repo, main, wt string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	main = gitRepo(t)
	// Local (not just per-exec env) identity: IntegrateTarget's `git merge` runs through the code
	// under test's own runGit, which does not set GIT_AUTHOR/COMMITTER — it relies on repo config,
	// same as a real user's checkout would.
	mergeFixtureRun(t, main, "config", "user.email", "t@t.com")
	mergeFixtureRun(t, main, "config", "user.name", "t")
	mergeFixtureWrite(t, main, "f.txt", "base\n", "base")
	r, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(r, "feature", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return r, main, dest
}

// mergeFixtureHooksVia checks that the main checkout's HEAD/index are untouched and no MERGE_HEAD
// exists there — the invariant Preflight/Merge must hold before promotion ever runs.
func mergeFixtureAssertMainUntouched(t *testing.T, main, wantHEAD string) {
	t.Helper()
	if got := headOf(main); got != wantHEAD {
		t.Fatalf("main HEAD = %s, want %s (main must stay untouched before promotion)", got, wantHEAD)
	}
	out, err := exec.Command("git", "-C", main, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("main checkout has uncommitted changes: %q", out)
	}
	gitDir, err := gitDirOf(context.Background(), main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); err == nil {
		t.Fatal("MERGE_HEAD exists in the main checkout after a predicted conflict")
	}
}

func TestPreflightFastForward(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", plan.Issues)
	}
	if plan.TargetRef != "main" {
		t.Fatalf("TargetRef = %q, want main", plan.TargetRef)
	}
	if plan.TargetOID != headOf(main) || plan.SourceOID != headOf(wt) {
		t.Fatalf("OIDs not pinned to live HEADs: plan=%+v", plan)
	}
}

func TestPreflightDivergentCleanMerge(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")
	mergeFixtureWrite(t, main, "h.txt", "main file\n", "main adds h")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Issues = %v, want none (non-overlapping changes)", plan.Issues)
	}
}

func TestPreflightTextConflict(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "feature line\n", "feature edits f")
	mergeFixtureWrite(t, main, "f.txt", "main line\n", "main edits f")
	mainHEAD := headOf(main)

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if !containsPath(plan.Issues[0].Paths, "f.txt") {
		t.Fatalf("conflict paths = %v, want f.txt", plan.Issues[0].Paths)
	}
	mergeFixtureAssertMainUntouched(t, main, mainHEAD)

	// Merge must refuse outright, never touching main or writing to the worktree either.
	_, _, err = Merge(context.Background(), repo, "feature", "")
	if err == nil {
		t.Fatal("Merge: want error on predicted conflict")
	}
	mergeFixtureAssertMainUntouched(t, main, mainHEAD)
}

func TestPreflightBinaryConflict(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "\x00\x01binA", "feature binary")
	mergeFixtureWrite(t, main, "f.txt", "\x00\x01binB", "main binary")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if !containsPath(plan.Issues[0].Paths, "f.txt") {
		t.Fatalf("conflict paths = %v, want f.txt", plan.Issues[0].Paths)
	}
}

func TestPreflightDeleteConflict(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "feature modify\n", "feature modifies f")
	mergeFixtureRemove(t, main, "f.txt", "main deletes f")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if !containsPath(plan.Issues[0].Paths, "f.txt") {
		t.Fatalf("conflict paths = %v, want f.txt", plan.Issues[0].Paths)
	}
}

func TestPreflightRenameConflict(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	// Rename/rename: same original file renamed to two different names on each side — merge-tree's
	// rename-aware three-way merge cannot pick one without a decision, unlike a rename plus a
	// non-conflicting edit (which it resolves silently).
	mergeFixtureRename(t, wt, "f.txt", "g.txt", "feature renames to g")
	mergeFixtureRename(t, main, "f.txt", "h.txt", "main renames to h")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if len(plan.Issues[0].Paths) == 0 {
		t.Fatal("rename/rename conflict reported no paths")
	}
}

func TestPreflightDirtyTarget(t *testing.T) {
	repo, main, _ := mergeFixtureSetup(t)
	if err := os.WriteFile(filepath.Join(main, "dirty.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueDirtyTarget {
		t.Fatalf("Issues = %v, want one IssueDirtyTarget", plan.Issues)
	}
	if plan.TargetOID != "" || plan.SourceOID != "" {
		t.Fatalf("OIDs pinned despite a dirty target: %+v", plan)
	}
}

// TestPreflightDirtyWorktreeWithShowUntrackedFilesOff is the regression test for Appendix A
// codex-review finding #8 (internal/worktree/preflight.go:216): a repo-local
// status.showUntrackedFiles=no must not let an untracked file hide a dirty worktree from Preflight —
// isDirty must explicitly request untracked files regardless of that config.
func TestPreflightDirtyWorktreeWithShowUntrackedFilesOff(t *testing.T) {
	repo, _, wt := mergeFixtureSetup(t)
	mergeFixtureRun(t, wt, "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("new file"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueDirtyWorktree {
		t.Fatalf("Issues = %v, want one IssueDirtyWorktree (an untracked file must still count as dirty)", plan.Issues)
	}
}

func TestPreflightDirtyWorktree(t *testing.T) {
	repo, _, wt := mergeFixtureSetup(t)
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueDirtyWorktree {
		t.Fatalf("Issues = %v, want one IssueDirtyWorktree", plan.Issues)
	}
}

func TestPreflightDetachedTarget(t *testing.T) {
	repo, main, _ := mergeFixtureSetup(t)
	head := headOf(main)
	mergeFixtureRun(t, main, "checkout", "-q", head)

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueDetachedTarget {
		t.Fatalf("Issues = %v, want one IssueDetachedTarget", plan.Issues)
	}
}

func TestPreflightTargetMismatch(t *testing.T) {
	repo, _, _ := mergeFixtureSetup(t)

	plan, err := Preflight(context.Background(), repo, "feature", "some-other-branch")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueTargetMismatch {
		t.Fatalf("Issues = %v, want one IssueTargetMismatch", plan.Issues)
	}
}

func TestPreflightInProgressOperation(t *testing.T) {
	repo, main, _ := mergeFixtureSetup(t)
	gitDir, err := gitDirOf(context.Background(), main)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte(headOf(main)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueInProgress {
		t.Fatalf("Issues = %v, want one IssueInProgress", plan.Issues)
	}
}

func TestPreflightUnrelatedHistories(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureRun(t, wt, "checkout", "-q", "--orphan", "orphan-base")
	mergeFixtureRun(t, wt, "commit", "-q", "--allow-empty", "-m", "orphan root")
	_ = main

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueUnrelatedHistories {
		t.Fatalf("Issues = %v, want one IssueUnrelatedHistories", plan.Issues)
	}
}

// TestInProgressOpPropagatesNonNotExistStatError is the regression test for Appendix A codex-review
// finding #9's inProgressOp half (internal/worktree/preflight.go:239): a stat failure that is NOT
// "file doesn't exist" (permission denied on the git-dir) must propagate as an error, never be
// silently read as "no operation in progress".
func TestInProgressOpPropagatesNonNotExistStatError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks don't apply when running as root")
	}
	_, _, wt := mergeFixtureSetup(t)
	gitDir, err := gitDirOf(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gitDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	if _, _, err := inProgressOp(context.Background(), wt); err == nil {
		t.Fatal("inProgressOp: want a propagated error for a permission-denied stat, got nil")
	}
}

func TestPreflightMissingWorktree(t *testing.T) {
	repo, _, _ := mergeFixtureSetup(t)
	if _, err := Preflight(context.Background(), repo, "does-not-exist", ""); err == nil {
		t.Fatal("want error for a nonexistent worktree")
	}
}

// TestSimulateMergeUnsupportedGit swaps runGit for a fake that reports merge-tree's historical absence
// (git < 2.38) as git itself would: a nonzero exit code outside {0,1}. Preflight must block, never
// mistake it for a clean or conflicted result.
func TestSimulateMergeUnsupportedGit(t *testing.T) {
	repo, _, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")

	orig := runGit
	runGit = func(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "merge-tree" {
			return nil, &exec.ExitError{ProcessState: mergeFixtureFakeExitState(129)}
		}
		return orig(ctx, dir, extraEnv, args...)
	}
	defer func() { runGit = orig }()

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueCapability {
		t.Fatalf("Issues = %v, want one IssueCapability", plan.Issues)
	}
}

// mergeFixtureFakeExitState runs a trivial subprocess to obtain a real *os.ProcessState with the
// wanted exit code — exec.ExitError requires one, and this is the only way to fabricate it honestly.
func mergeFixtureFakeExitState(code int) *os.ProcessState {
	cmd := exec.Command("sh", "-c", "exit "+itoa(code))
	_ = cmd.Run()
	return cmd.ProcessState
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}
