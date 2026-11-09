package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestCreateDefaultBranchNaming(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "feature", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dest, "branch", "--show-current")
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "worktree-feature" {
		t.Fatalf("branch = %q, err %v, want worktree-feature", out, err)
	}
}

func TestCreateCustomBranchName(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "feature", CreateOptions{Branch: "wt/custom-name"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dest, "branch", "--show-current")
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "wt/custom-name" {
		t.Fatalf("branch = %q, err %v, want wt/custom-name", out, err)
	}
}

func TestCreateReusesExistingBranch(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	brCmd := exec.Command("git", "-C", repo.Main, "branch", "wt/preexisting")
	brCmd.Env = gitEnv()
	if out, err := brCmd.CombinedOutput(); err != nil {
		t.Fatalf("branch: %v: %s", err, out)
	}
	dest, err := Create(repo, "reuse", CreateOptions{Branch: "wt/preexisting"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dest, "branch", "--show-current")
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "wt/preexisting" {
		t.Fatalf("branch = %q, err %v, want wt/preexisting", out, err)
	}
}

func TestCreateIdempotentResume(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest1, err := Create(repo, "dup", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dest2, err := Create(repo, "dup", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dest1 != dest2 {
		t.Fatalf("paths differ: %q vs %q", dest1, dest2)
	}
}

func TestCreateRejectsTraversalName(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	if _, err := Create(repo, "../escape", CreateOptions{}); err == nil {
		t.Fatal("want error for a traversal name")
	}
}

func TestCreateUnusualFilenames(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	for _, name := range []string{"dots.v2", "under_score-123", "unicode-éé"} {
		dest, err := Create(repo, name, CreateOptions{})
		if err != nil {
			t.Errorf("name %q: %v", name, err)
			continue
		}
		if _, err := os.Stat(dest); err != nil {
			t.Errorf("name %q: worktree missing at %s: %v", name, dest, err)
		}
	}
}

// TestCreateSpaceInNameDerivesInvalidBranchRef documents a code defect: ResolveWorktree accepts any
// name without a path separator (including spaces), but Create's default "worktree-"+name branch is
// not a valid git ref when name contains a space, so Create fails with git's raw stderr instead of a
// clear error or a sanitized branch name. See internal/worktree/lifecycle.go:24.
func TestCreateSpaceInNameDerivesInvalidBranchRef(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	_, err := Create(repo, "has space", CreateOptions{})
	if err == nil {
		t.Fatal("BUG regression: Create used to fail on a derived branch name containing a space; it now succeeds — if Create was fixed to sanitize the default branch name, update this test to assert success instead")
	}
	if !strings.Contains(err.Error(), "not a valid ref") && !strings.Contains(err.Error(), "not a valid branch name") {
		t.Fatalf("got a different error than expected, re-triage: %v", err)
	}
}

func TestCleanRefusesDirty(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "dirty", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Clean(repo, "dirty", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed {
		t.Fatal("want refusal for a dirty worktree")
	}
	if !strings.Contains(result.Reason, "uncommitted") {
		t.Errorf("Reason = %q, want mention of uncommitted changes", result.Reason)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("worktree should still exist after a refused clean")
	}
}

func TestCleanRefusesUnmerged(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "ahead", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "unmerged")
	result, err := Clean(repo, "ahead", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed {
		t.Fatal("want refusal for an unmerged worktree")
	}
	if !strings.Contains(result.Reason, "not merged") {
		t.Errorf("Reason = %q, want mention of merge state", result.Reason)
	}
}

func TestCleanRemovesMergedAndDeletesBranch(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	main := repo.Main
	if _, err := Create(repo, "clean", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err := Clean(repo, "clean", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Removed {
		t.Fatalf("want removal, got refusal: %s", result.Reason)
	}
	if !result.BranchDeleted {
		t.Error("want the fully-merged branch deleted")
	}
	out, err := exec.Command("git", "-C", main, "branch", "--list", "worktree-clean").Output()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("branch should be gone, branch --list = %q, err %v", out, err)
	}
}

func TestCleanDryRunThenRealRemovalRechecksAfterDirtyChange(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "drifts", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Preview says eligible while the tree is still clean.
	preview, err := Clean(repo, "drifts", true)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Removed {
		t.Fatalf("preview should report eligible, got refusal: %s", preview.Reason)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("dry run must never remove anything")
	}

	// The tree changes between preview and the real call — the real call must recheck live, not trust the preview.
	if err := os.WriteFile(filepath.Join(dest, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := Clean(repo, "drifts", false)
	if err != nil {
		t.Fatal(err)
	}
	if real.Removed {
		t.Fatal("the real call must refuse now that the tree went dirty after the preview")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("worktree should still be present after the refused real call")
	}
}

func TestCleanMissingWorktree(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	if _, err := Clean(repo, "never-existed", false); err == nil {
		t.Fatal("want error for a name with no worktree on disk")
	}
}

func TestCleanLockedRefused(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	main := repo.Main
	dest, err := Create(repo, "locked", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", main, "worktree", "lock", dest).CombinedOutput(); err != nil {
		t.Fatalf("worktree lock: %v: %s", err, out)
	}
	result, err := Clean(repo, "locked", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed {
		t.Fatal("want refusal for a locked worktree")
	}
	if !strings.Contains(result.Reason, "locked") {
		t.Errorf("Reason = %q, want mention of lock state", result.Reason)
	}
}

// requireOneRemovedOneRefused fails unless results holds exactly one removed and one refused entry.
func requireOneRemovedOneRefused(t *testing.T, results []CleanResult) {
	t.Helper()
	removed, refused := 0, 0
	for _, r := range results {
		if r.Removed {
			removed++
		} else {
			refused++
		}
	}
	if removed != 1 || refused != 1 {
		t.Fatalf("removed=%d refused=%d, want 1 and 1", removed, refused)
	}
}

func TestCleanAllPreviewThenApply(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	if _, err := Create(repo, "mergeable", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dest2, err := Create(repo, "ahead", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, dest2, "commit", "--allow-empty", "-q", "-m", "stays")

	preview, err := CleanAll(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 2 {
		t.Fatalf("got %d preview results, want 2", len(preview))
	}
	for _, r := range preview {
		if _, err := os.Stat(r.Path); err != nil {
			t.Errorf("preview must not remove anything, %s missing: %v", r.Path, err)
		}
	}

	applied, err := CleanAll(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	requireOneRemovedOneRefused(t, applied)
	if _, err := os.Stat(dest2); err != nil {
		t.Fatal("the unmerged worktree must survive clean-all --yes")
	}
}

// gitDirOfForTest resolves dest's own private git-dir the same way the production code does, for a
// test that needs to drop an operation marker directly into it.
func gitDirOfForTest(t *testing.T, dest string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dest, "rev-parse", "--path-format=absolute", "--git-dir")
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// TestCleanRefusesPendingAutostash is the regression test for Appendix A codex-review finding #5
// (internal/worktree/lifecycle.go:83): a pending merge that used --autostash moves the worktree's own
// local edits into the stash and leaves MERGE_HEAD/MERGE_AUTOSTASH set — `git status --porcelain`
// reports clean (the edits are no longer in the working tree at all), so without an explicit
// in-progress-operation check, Clean would otherwise remove the worktree and strand the only pointer
// to those stashed edits held in its own private git-dir.
func TestCleanRefusesPendingAutostash(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "autostashed", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gitDir := gitDirOfForTest(t, dest)
	// Simulate an interrupted `git merge --autostash`: both markers present, working tree otherwise clean.
	if err := os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte(headOf(dest)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "MERGE_AUTOSTASH"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Clean(repo, "autostashed", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed {
		t.Fatal("Clean: want refusal while a merge with a pending autostash restore is in progress")
	}
	if !strings.Contains(result.Reason, "autostash") && !strings.Contains(result.Reason, "merge") {
		t.Errorf("Reason = %q, want it to mention the in-progress merge/autostash", result.Reason)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("worktree (and its only reference to the stashed edits) must survive a refused clean")
	}
}

// TestIsLockedPropagatesNonNotExistStatError is the regression test for Appendix A codex-review
// finding #9's isLocked half (internal/worktree/preflight.go:239, "isLocked similarly reports
// unlocked on inspection errors"): a stat failure that is NOT "file doesn't exist" (here, permission
// denied on the containing directory) must propagate as an error, never be read as "unlocked".
func TestIsLockedPropagatesNonNotExistStatError(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission checks don't apply when running as root")
	}
	repo := testRepo(t)
	dest, err := Create(repo, "permcheck", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gitDir := gitDirOfForTest(t, dest)
	if err := os.Chmod(gitDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) }) // TempDir cleanup needs this back

	locked, lockErr := isLocked(dest)
	if lockErr == nil {
		t.Fatalf("isLocked: want a propagated error for a permission-denied stat, got locked=%v, nil error", locked)
	}
}

// TestCleanRefusesInUseAndDryRunReportsIt is the regression test for the busy-cwd guard: Clean must
// refuse a worktree InUseBy reports as busy, under both the real call and --dry-run's preview.
func TestCleanRefusesInUseAndDryRunReportsIt(t *testing.T) {
	repo := testRepo(t)
	dest, err := Create(repo, "busy", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stubProcesses(t, func(dir string) ([]Proc, error) {
		return []Proc{{PID: 999999, Comm: "fakeproc"}}, nil
	})

	dry, err := Clean(repo, "busy", true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Removed {
		t.Fatal("dry-run must report refusal for a busy worktree, not eligibility")
	}
	if !strings.Contains(dry.Reason, "999999") {
		t.Errorf("dry-run Reason = %q, want the busy pid named", dry.Reason)
	}

	real, err := Clean(repo, "busy", false)
	if err != nil {
		t.Fatal(err)
	}
	if real.Removed {
		t.Fatal("want refusal for a busy worktree")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("worktree should still exist after a refused clean")
	}
}

// TestCleanAllSkipsBusyEntryButRemovesOthers proves CleanAll continues past a busy refusal rather
// than aborting the whole sweep, and still removes every other eligible entry.
func TestCleanAllSkipsBusyEntryButRemovesOthers(t *testing.T) {
	repo := testRepo(t)
	busyDest, err := Create(repo, "busyall", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(repo, "freeall", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	stubProcesses(t, func(dir string) ([]Proc, error) {
		if paths.Within(busyDest, dir) {
			return []Proc{{PID: 999999, Comm: "fakeproc"}}, nil
		}
		return nil, nil
	})

	results, err := CleanAll(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if !r.Removed && !strings.Contains(r.Reason, "999999") {
			t.Errorf("refused entry Reason = %q, want the busy pid named", r.Reason)
		}
	}
	requireOneRemovedOneRefused(t, results)
	if _, err := os.Stat(busyDest); err != nil {
		t.Fatal("the busy worktree must survive clean-all")
	}
}

func TestCleanAllSkipsOrphanedEntries(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	if err := os.MkdirAll(filepath.Join(repo.Root, "stray"), 0o755); err != nil {
		t.Fatal(err)
	}
	results, err := CleanAll(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if filepath.Base(r.Path) == "stray" {
			t.Fatal("CleanAll must never touch an orphaned, unregistered directory")
		}
	}
}

func forceFixture(t *testing.T) (repo *Repo, main, dest string) {
	t.Helper()
	repo = testRepo(t)
	main = repo.Main
	dest, err := Create(repo, "force", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "unmerged")
	if err := os.WriteFile(filepath.Join(dest, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, main, dest
}

func TestForceCleanRefusesDirtyUnmergedWithoutForce(t *testing.T) {
	t.Parallel()
	repo, _, dest := forceFixture(t)
	res, err := Clean(repo, "force", false)
	if err != nil || res.Removed {
		t.Fatalf("plain Clean must refuse: removed=%v err=%v", res.Removed, err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("worktree must remain")
	}
}

func TestForceCleanDiscardsDirtyUnmergedAndReports(t *testing.T) {
	t.Parallel()
	repo, main, dest := forceFixture(t)
	var got []DiscardSummary
	res, err := CleanForce(repo, "force", false, func(s DiscardSummary) { got = append(got, s) })
	if err != nil || !res.Removed || !res.BranchDeleted {
		t.Fatalf("want removed+branch deleted: %+v err=%v", res, err)
	}
	if len(got) != 1 || got[0].Dirty != 1 || got[0].Unmerged != 1 || got[0].Branch != "worktree-force" {
		t.Fatalf("summary = %+v", got)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("dir still exists")
	}
	if branchExists(main, "worktree-force") {
		t.Fatal("branch still exists")
	}
}

func TestForceCleanDryRunReportsButKeeps(t *testing.T) {
	t.Parallel()
	repo, main, dest := forceFixture(t)
	called := 0
	res, err := CleanForce(repo, "force", true, func(DiscardSummary) { called++ })
	if err != nil || !res.Removed || called != 1 {
		t.Fatalf("dry-run: %+v err=%v called=%d", res, err, called)
	}
	if _, err := os.Stat(dest); err != nil || !branchExists(main, "worktree-force") {
		t.Fatal("dry-run must not remove anything")
	}
}

func TestForceCleanRefusesInUse(t *testing.T) {
	repo, main, dest := forceFixture(t)
	stubProcesses(t, func(string) ([]Proc, error) { return []Proc{{PID: 999999, Comm: "fakeproc"}}, nil })
	called := false
	res, err := CleanForce(repo, "force", false, func(DiscardSummary) { called = true })
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed || !strings.Contains(res.Reason, "999999") {
		t.Fatalf("want in-use refusal naming pid, got %+v", res)
	}
	if called {
		t.Error("nothing should be reported as discarded when refused")
	}
	if _, err := os.Stat(dest); err != nil || !branchExists(main, "worktree-force") {
		t.Fatal("refused force must leave worktree and branch")
	}
}

func TestForceCleanNeverRemovesMainCheckout(t *testing.T) {
	t.Parallel()
	repo, main, _ := forceFixture(t)
	if !isMainCheckout(repo, main) {
		t.Fatal("isMainCheckout must recognise the main checkout")
	}
	for _, name := range []string{"..", ".", filepath.Base(main), "../" + filepath.Base(main)} {
		res, err := CleanForce(repo, name, false, nil)
		if err == nil && res.Removed {
			t.Errorf("name %q: main checkout removed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(main, ".git")); err != nil {
		t.Fatalf("main damaged: %v", err)
	}
}

func TestContextAwareWorktreeLookupsReturnCancellation(t *testing.T) {
	t.Parallel()
	repo, _, wt := preflightFixtureSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := resolveWorktree(ctx, repo, filepath.Base(wt)); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveWorktree error = %v, want context.Canceled", err)
	}
	if _, err := ownsWorktree(ctx, repo, wt); !errors.Is(err, context.Canceled) {
		t.Fatalf("ownsWorktree error = %v, want context.Canceled", err)
	}
	if _, err := headOfContext(ctx, wt); !errors.Is(err, context.Canceled) {
		t.Fatalf("headOfContext error = %v, want context.Canceled", err)
	}
}

// TestPromoteCancelledContextNeverMovesMain covers Promote's own early ctx.Err() check: a context
// already cancelled when Promote is entered must return immediately without acquiring the merge lock
// or touching repo.Main at all — not even a failed fast-forward attempt.
func TestPromoteCancelledContextNeverMovesMain(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	mainBefore := headOf(main)

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := IntegrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Promote(ctx, repo, plan); err == nil {
		t.Fatal("Promote: want an error for an already-cancelled context")
	}
	if headOf(main) != mainBefore {
		t.Fatalf("main HEAD = %s, want unchanged %s — a cancelled ctx must never move it", headOf(main), mainBefore)
	}
}

// TestPromoteCancelledAfterChecksStillRefusesFastForward covers the second ctx.Err() check inside
// promoteLocked, right before the real `git merge --ff-only` runs: even once every recheck (branch,
// dirty, OIDs, ancestry, re-simulation) has already passed, a context cancelled in that window must
// still stop before the ff-only update, never racing a late cancellation into a real mutation.
func TestPromoteCancelledAfterChecksStillRefusesFastForward(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	mainBefore := headOf(main)

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := IntegrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	orig := runGit
	runGit = func(ctx2 context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
		if slices.Contains(args, "--ff-only") {
			cancel()
		}
		return orig(ctx2, dir, extraEnv, args...)
	}
	defer func() { runGit = orig }()

	if _, err := Promote(ctx, repo, plan); err == nil {
		t.Fatal("Promote: want an error — ctx was cancelled just before the ff-only merge")
	}
	if headOf(main) != mainBefore {
		t.Fatalf("main HEAD = %s, want unchanged %s", headOf(main), mainBefore)
	}
}

// TestRunGitCancelledContextReturnsErrorNeverFalseClean covers the one seam over external git:
// runGit's production implementation wraps exec.CommandContext, so a cancelled ctx must surface as an
// error from the git call itself — never silently read as git's own "clean" exit code (0) by a caller
// like isAncestor/unrelatedHistories that distinguishes exit-code 1 from "everything else".
func TestRunGitCancelledContextReturnsErrorNeverFalseClean(t *testing.T) {
	t.Parallel()
	_, main, _ := preflightFixtureSetup(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runGit(ctx, main, nil, "status", "--porcelain")
	if err == nil {
		t.Fatal("runGit: want an error for an already-cancelled context, not a successful empty result")
	}
}

// TestResolvePollingCancelledReturnsPromptlyPreservingStateThenResumeSucceeds covers --resume's poll
// loop (pollUntilResolved): a context cancelled mid-poll must return promptly (not wait out
// maxResolveWait), preserve the operation file and the worktree exactly as they were, and a later
// --resume (a fresh, uncancelled call) must still be able to pick the resolution up and succeed.
func TestResolvePollingCancelledReturnsPromptlyPreservingStateThenResumeSucceeds(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Minute) // long enough that a real timeout would never fire first

	materialized, err := materializeConflict(context.Background(), plan)
	if err != nil {
		t.Fatalf("materializeConflict: %v", err)
	}
	if materialized.Phase != "conflict" {
		t.Fatalf("materialized.Phase = %q, want %q", materialized.Phase, "conflict")
	}

	op := &Operation{
		ID:                "op-cancel-poll",
		Repo:              repo.Main,
		Worktree:          filepath.Base(wt),
		ConflictPaths:     []string{"f.txt"},
		TargetRef:         plan.TargetRef,
		TargetOID:         plan.TargetOID,
		OriginalSourceOID: plan.SourceOID,
		Phase:             "resolving",
		StartedAt:         nowFn(),
		Deadline:          nowFn().Add(time.Minute),
		Agent:             "claude",
		AgentExitOK:       true,
	}
	if err := SaveOperation(op); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	reloaded := resolveCancelledPoll(t, repo, plan, op, wt)
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
	resolveResumeAfterCancel(t, repo, plan, reloaded, wt)
}

// resolveCancelledPoll resumes op with an already-cancelled context, asserts the prompt cancelled return with the
// operation record and MERGE_HEAD preserved, and returns the reloaded operation.
func resolveCancelledPoll(t *testing.T, repo *Repo, plan *MergePlan, op *Operation, wt string) *Operation {
	t.Helper()
	// The worktree is still genuinely mid-merge (MERGE_HEAD set, unmerged f.txt) — the poll loop must
	// never move past "pending" on its own; only the ctx cancellation below ends this call.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the very first loop iteration's ctx.Err() check must fire

	start := time.Now()
	gotOp, result, err := ResolveWithAgent(ctx, repo, plan, "claude", op)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ResolveWithAgent (resume, cancelled): want an error")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on cancellation", result)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("ResolveWithAgent took %s to return after cancellation, want it to stop promptly", elapsed)
	}
	if gotOp.Phase != "cancelled" {
		t.Fatalf("gotOp.Phase = %q, want %q", gotOp.Phase, "cancelled")
	}
	reloaded, loadErr := LoadOperation(repo, "feature")
	if loadErr != nil || reloaded.Phase != "cancelled" {
		t.Fatalf("reloaded = %+v, err=%v, want the preserved cancelled record", reloaded, loadErr)
	}
	mh, mhErr := hasMergeHead(context.Background(), wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if !mh {
		t.Fatal("MERGE_HEAD was cleaned up after cancellation — no automatic cleanup is permitted")
	}
	return reloaded
}

// resolveResumeAfterCancel resolves the conflict for real and asserts a live --resume finishes the job.
func resolveResumeAfterCancel(t *testing.T, repo *Repo, plan *MergePlan, reloaded *Operation, wt string) {
	t.Helper()
	// A later --resume with a live (uncancelled) context must still be able to finish the job: resolve
	// the conflict for real now, matching what CheckResolution's own poll would have found.
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("resolved after cancel\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mergeFixtureRun(t, wt, "add", "f.txt")
	mergeFixtureRun(t, wt, "commit", "--no-edit")

	resumedOp, resumedResult, resumeErr := ResolveWithAgent(context.Background(), repo, plan, "claude", reloaded)
	if resumeErr != nil {
		t.Fatalf("ResolveWithAgent (resume after cancel): %v", resumeErr)
	}
	if resumedResult == nil || resumedResult.Phase != "integrate" {
		t.Fatalf("resumedResult = %+v, want a completed integrate", resumedResult)
	}
	if resumedOp.Phase != "resolved" {
		t.Fatalf("resumedOp.Phase = %q, want %q", resumedOp.Phase, "resolved")
	}
}

// TestCleanRefusesWhileWorktreeLeaseHeld covers Clean's own WithWorktreeLease hold (lifecycle.go): an
// ordinary clean must refuse outright, before ever rechecking the worktree's state, while another
// operation already holds the same worktree's lease — even one that would otherwise be eligible.
func TestCleanRefusesWhileWorktreeLeaseHeld(t *testing.T) {
	t.Parallel()
	repo, main, _ := mergeFixtureSetup(t)
	mergeFixtureWrite(t, main, "h.txt", "main advances past the merged worktree\n", "main advances")

	if err := os.MkdirAll(operationsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = paths.TryFileLock(operationLeasePath(repo.Main, "feature"), 2*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	_, err := Clean(repo, "feature", false)
	if err == nil {
		t.Fatal("Clean: want an error while another operation holds the worktree lease")
	}
	if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("err = %q, want it to report the lease conflict", err)
	}
}
