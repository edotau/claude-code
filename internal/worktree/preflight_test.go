package worktree

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
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
	cmd.Env = gitEnv(
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// preflightFixtureSetup makes an isolated main repo with a base file commit plus a "feature" worktree
// without mutating process env, so read-only/worktree-local tests can run in parallel.
func preflightFixtureSetup(t *testing.T) (repo *Repo, main, wt string) {
	t.Helper()
	r := testRepo(t)
	main = r.Main
	// The gittest template carries a local identity, so integrateTarget's `git merge` (no GIT_AUTHOR env) can commit.
	mergeFixtureWrite(t, main, "f.txt", "base\n", "base")
	dest, err := Create(r, "feature", CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return r, main, dest
}

// mergeFixtureAssertMainUntouched checks that the main checkout's HEAD/index are untouched and no MERGE_HEAD
// exists there — the invariant Preflight/Merge must hold before promotion ever runs.
func mergeFixtureAssertMainUntouched(t *testing.T, main, wantHEAD string) {
	t.Helper()
	if got := headOf(main); got != wantHEAD {
		t.Fatalf("main HEAD = %s, want %s (main must stay untouched before promotion)", got, wantHEAD)
	}
	stCmd := exec.Command("git", "-C", main, "status", "--porcelain")
	stCmd.Env = gitEnv()
	out, err := stCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("main checkout has uncommitted changes: %q", out)
	}
	gitDir, _, err := gitDirsOf(context.Background(), main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); err == nil {
		t.Fatal("MERGE_HEAD exists in the main checkout after a predicted conflict")
	}
}

func TestScreenCheckoutsStopsOnTargetMismatch(t *testing.T) {
	repo := testRepo(t)
	plan := &MergePlan{WorktreePath: filepath.Join(t.TempDir(), "missing-worktree")}
	stop, err := screenCheckouts(context.Background(), repo, plan, "release", false)
	if err != nil || !stop {
		t.Fatalf("target mismatch must stop before inspecting the nonexistent worktree: stop=%v err=%v", stop, err)
	}
	if plan.TargetRef != "main" || len(plan.Issues) != 1 {
		t.Fatalf("branch screen did not preserve target and issue: %+v", plan)
	}
	issue := plan.Issues[0]
	if issue.Code != issueTargetMismatch || !strings.Contains(issue.Detail, `requested target was "release"`) {
		t.Fatalf("issue = %+v, want requested-target mismatch", issue)
	}
	if plan.TargetOID != "" || plan.SourceOID != "" {
		t.Fatalf("mismatched target must not pin OIDs: %+v", plan)
	}
}

func TestPreflightFastForward(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
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
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
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
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
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
	if !slices.Contains(plan.Issues[0].Paths, "f.txt") {
		t.Fatalf("conflict paths = %v, want f.txt", plan.Issues[0].Paths)
	}
	mergeFixtureAssertMainUntouched(t, main, mainHEAD)
}

func TestPreflightBinaryConflict(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "\x00\x01binA", "feature binary")
	mergeFixtureWrite(t, main, "f.txt", "\x00\x01binB", "main binary")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if !slices.Contains(plan.Issues[0].Paths, "f.txt") {
		t.Fatalf("conflict paths = %v, want f.txt", plan.Issues[0].Paths)
	}
}

func TestPreflightDeleteConflict(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "feature modify\n", "feature modifies f")
	mergeFixtureRemove(t, main, "f.txt", "main deletes f")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if !slices.Contains(plan.Issues[0].Paths, "f.txt") {
		t.Fatalf("conflict paths = %v, want f.txt", plan.Issues[0].Paths)
	}
}

func TestPreflightRenameConflict(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
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

func assertDirtyTargetIssue(t *testing.T, plan *MergePlan, path string) {
	t.Helper()
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueDirtyTarget {
		t.Fatalf("Issues = %v, want one IssueDirtyTarget", plan.Issues)
	}
	issue := plan.Issues[0]
	if !slices.Equal(issue.Paths, []string{path}) || !strings.Contains(issue.Detail, path) {
		t.Fatalf("issue = %+v, want only overlapping path %q and detail naming it", issue, path)
	}
}

func TestPreflightDirtyTargetNonOverlapping(t *testing.T) {
	for _, name := range []string{"f.txt", "dirty\nfile.txt"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, main, wt := preflightFixtureSetup(t)
			mergeFixtureWrite(t, main, "f.txt", "main edit\n", "main edits f")
			mergeFixtureWrite(t, wt, "g.txt", "feature\n", "feature adds g")
			if err := os.WriteFile(filepath.Join(main, name), []byte("uncommitted"), 0o644); err != nil {
				t.Fatal(err)
			}
			plan, err := Preflight(context.Background(), repo, "feature", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Issues) != 0 || plan.TargetOID != headOf(main) || plan.SourceOID != headOf(wt) {
				t.Fatalf("non-overlapping dirty target blocked or OIDs not pinned: %+v", plan)
			}
		})
	}
}

func TestPreflightDirtyTargetRename(t *testing.T) {
	for _, name := range []string{"f.txt", "renamed\nfile.txt"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, main, wt := preflightFixtureSetup(t)
			mergeFixtureWrite(t, wt, name, "feature\n", "feature edits path")
			mergeFixtureRun(t, main, "mv", "f.txt", "renamed\nfile.txt")
			plan, err := Preflight(context.Background(), repo, "feature", "")
			if err != nil {
				t.Fatal(err)
			}
			assertDirtyTargetIssue(t, plan, name)
		})
	}
}

func TestPreflightPromotedRenameOverlapsOldPath(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureRename(t, wt, "f.txt", "renamed.txt", "feature renames f")
	if err := os.WriteFile(filepath.Join(main, "f.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatal(err)
	}
	assertDirtyTargetIssue(t, plan, "f.txt")
}

// TestPreflightDirtyWorktreeWithShowUntrackedFilesOff is the regression test for Appendix A
// codex-review finding #8 (internal/worktree/preflight.go:216): a repo-local
// status.showUntrackedFiles=no must not let an untracked file hide a dirty worktree from Preflight —
// isDirty must explicitly request untracked files regardless of that config.
func TestPreflightDirtyWorktreeWithShowUntrackedFilesOff(t *testing.T) {
	t.Parallel()
	repo, _, wt := preflightFixtureSetup(t)
	mergeFixtureRun(t, wt, "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("new file"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueDirtyWorktree {
		t.Fatalf("Issues = %v, want one IssueDirtyWorktree (an untracked file must still count as dirty)", plan.Issues)
	}
}

func TestPreflightDirtyWorktree(t *testing.T) {
	t.Parallel()
	repo, _, wt := preflightFixtureSetup(t)
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueDirtyWorktree {
		t.Fatalf("Issues = %v, want one IssueDirtyWorktree", plan.Issues)
	}
}

func TestPreflightDetachedTarget(t *testing.T) {
	t.Parallel()
	repo, main, _ := preflightFixtureSetup(t)
	head := headOf(main)
	mergeFixtureRun(t, main, "checkout", "-q", head)

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueDetachedTarget {
		t.Fatalf("Issues = %v, want one IssueDetachedTarget", plan.Issues)
	}
}

func TestPreflightTargetMismatch(t *testing.T) {
	t.Parallel()
	repo, _, _ := preflightFixtureSetup(t)

	plan, err := Preflight(context.Background(), repo, "feature", "some-other-branch")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueTargetMismatch {
		t.Fatalf("Issues = %v, want one IssueTargetMismatch", plan.Issues)
	}
}

func TestPreflightInProgressOperation(t *testing.T) {
	t.Parallel()
	repo, main, _ := preflightFixtureSetup(t)
	if err := os.WriteFile(filepath.Join(main, "dirty.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDir, _, err := gitDirsOf(context.Background(), main)
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
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueInProgress {
		t.Fatalf("Issues = %v, want one IssueInProgress", plan.Issues)
	}
}

func TestPreflightUnrelatedHistories(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureRun(t, wt, "checkout", "-q", "--orphan", "orphan-base")
	mergeFixtureRun(t, wt, "commit", "-q", "--allow-empty", "-m", "orphan root")
	_ = main

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueUnrelatedHistories {
		t.Fatalf("Issues = %v, want one IssueUnrelatedHistories", plan.Issues)
	}
}

// TestInProgressOpPropagatesNonNotExistStatError is the regression test for Appendix A codex-review
// finding #9's inProgressOp half (internal/worktree/preflight.go:239): a stat failure that is NOT
// "file doesn't exist" (permission denied on the git-dir) must propagate as an error, never be
// silently read as "no operation in progress".
func TestInProgressOpPropagatesNonNotExistStatError(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission checks don't apply when running as root")
	}
	_, _, wt := preflightFixtureSetup(t)
	gitDir, _, err := gitDirsOf(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gitDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	if _, _, err := inProgressOp(context.Background(), wt, ""); err == nil {
		t.Fatal("inProgressOp: want a propagated error for a permission-denied stat, got nil")
	}
}

func TestPreflightMissingWorktree(t *testing.T) {
	t.Parallel()
	repo, _, _ := preflightFixtureSetup(t)
	if _, err := Preflight(context.Background(), repo, "does-not-exist", ""); err == nil {
		t.Fatal("want error for a nonexistent worktree")
	}
}

// TestSimulateMergeUnsupportedGit swaps runGit for a fake that reports merge-tree's historical absence
// (git < 2.38) as git itself would: a nonzero exit code outside {0,1}. Preflight must block, never
// mistake it for a clean or conflicted result.
func TestSimulateMergeUnsupportedGit(t *testing.T) {
	repo, _, wt := preflightFixtureSetup(t)
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
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueCapability {
		t.Fatalf("Issues = %v, want one IssueCapability", plan.Issues)
	}
}

// mergeFixtureFakeExitState runs a trivial subprocess to obtain a real *os.ProcessState with the
// wanted exit code — exec.ExitError requires one, and this is the only way to fabricate it honestly.
func mergeFixtureFakeExitState(code int) *os.ProcessState {
	cmd := exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	_ = cmd.Run()
	return cmd.ProcessState
}

// TestPlanOrResolveConflictNotifiesLedgerOwner: a blocked merge queues one conflict notice for the owning session.
func TestPlanOrResolveConflictNotifiesLedgerOwner(t *testing.T) {
	repo := conflictedOwnedWorktree(t, "owner-S")
	req := MergeRequest{Name: "feat", Err: io.Discard, ShowPlan: func(*MergePlan) {}}
	if plan, err := PlanOrResolve(context.Background(), repo, req, true); plan != nil || err != nil {
		t.Fatalf("dry run = %v, %v; want blocked", plan, err)
	}
	if got := Drain("owner-S"); len(got) != 0 {
		t.Fatalf("dry run queued %+v, want nothing", got)
	}
	if plan, err := PlanOrResolve(context.Background(), repo, req, false); plan != nil || err != nil {
		t.Fatalf("merge = %v, %v; want blocked", plan, err)
	}
	got := Drain("owner-S")
	if len(got) != 1 || got[0].Kind != "conflict" || !strings.Contains(got[0].Text, "conflict in f.txt") {
		t.Fatalf("owner notices = %+v, want one conflict notice naming f.txt", got)
	}
}

// conflictedOwnedWorktree is a repo whose worktree "feat", opened by session, conflicts with main on f.txt.
func conflictedOwnedWorktree(t *testing.T, session string) *Repo {
	t.Helper()
	home := paths.Canon(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home+"/.claude")
	main := gitRepo(t)
	mergeFixtureWrite(t, main, "f.txt", "base\n", "base")
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := Create(repo, "feat", CreateOptions{Session: session, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	mergeFixtureWrite(t, wt, "f.txt", "feature\n", "feature edits f")
	mergeFixtureWrite(t, main, "f.txt", "main\n", "main edits f")
	return repo
}

// TestPreflightDirtyTargetNamesClaimHolder: an overlapping dirty file names the live session that claims it.
func TestPreflightDirtyTargetNamesClaimHolder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "feature edit\n", "feature edits f")
	if err := os.WriteFile(filepath.Join(main, "f.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	claims := filepath.Join(ClaimsDir(), fmt.Sprintf("%d-peer-session.jsonl", os.Getpid()))
	if err := os.MkdirAll(filepath.Dir(claims), 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"schema":1,"path":%q,"ts":1}`+"\n", filepath.Join(main, "f.txt"))
	if err := os.WriteFile(claims, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatal(err)
	}
	assertDirtyTargetIssue(t, plan, "f.txt")
	if plan.TargetOID != "" || plan.SourceOID != "" {
		t.Fatalf("OIDs pinned despite an overlapping dirty target: %+v", plan)
	}
	want := fmt.Sprintf("f.txt (held by session peer-ses (pid %d))", os.Getpid())
	if !strings.Contains(plan.Issues[0].Detail, want) {
		t.Fatalf("detail = %q, want %q", plan.Issues[0].Detail, want)
	}
}
