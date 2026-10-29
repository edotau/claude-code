package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	removed, refused := 0, 0
	for _, r := range applied {
		if r.Removed {
			removed++
		} else {
			refused++
		}
	}
	if removed != 1 || refused != 1 {
		t.Fatalf("removed=%d refused=%d, want 1 and 1", removed, refused)
	}
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
		if within(busyDest, dir) {
			return []Proc{{PID: 999999, Comm: "fakeproc"}}, nil
		}
		return nil, nil
	})

	results, err := CleanAll(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	removed, refused := 0, 0
	for _, r := range results {
		if r.Removed {
			removed++
		} else {
			refused++
			if !strings.Contains(r.Reason, "999999") {
				t.Errorf("refused entry Reason = %q, want the busy pid named", r.Reason)
			}
		}
	}
	if removed != 1 || refused != 1 {
		t.Fatalf("removed=%d refused=%d, want 1 and 1", removed, refused)
	}
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
