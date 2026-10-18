package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateDefaultBranchNaming(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "feature", "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", dest, "branch", "--show-current").Output()
	if err != nil || strings.TrimSpace(string(out)) != "worktree-feature" {
		t.Fatalf("branch = %q, err %v, want worktree-feature", out, err)
	}
}

func TestCreateCustomBranchName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "feature", "wt/custom-name")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", dest, "branch", "--show-current").Output()
	if err != nil || strings.TrimSpace(string(out)) != "wt/custom-name" {
		t.Fatalf("branch = %q, err %v, want wt/custom-name", out, err)
	}
}

func TestCreateReusesExistingBranch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", main, "branch", "wt/preexisting").CombinedOutput(); err != nil {
		t.Fatalf("branch: %v: %s", err, out)
	}
	dest, err := Create(repo, "reuse", "wt/preexisting")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", dest, "branch", "--show-current").Output()
	if err != nil || strings.TrimSpace(string(out)) != "wt/preexisting" {
		t.Fatalf("branch = %q, err %v, want wt/preexisting", out, err)
	}
}

func TestCreateIdempotentResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest1, err := Create(repo, "dup", "")
	if err != nil {
		t.Fatal(err)
	}
	dest2, err := Create(repo, "dup", "")
	if err != nil {
		t.Fatal(err)
	}
	if dest1 != dest2 {
		t.Fatalf("paths differ: %q vs %q", dest1, dest2)
	}
}

func TestCreateRejectsTraversalName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(repo, "../escape", ""); err == nil {
		t.Fatal("want error for a traversal name")
	}
}

func TestCreateUnusualFilenames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dots.v2", "under_score-123", "unicode-éé"} {
		dest, err := Create(repo, name, "")
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
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Create(repo, "has space", "")
	if err == nil {
		t.Fatal("BUG regression: Create used to fail on a derived branch name containing a space; it now succeeds — if Create was fixed to sanitize the default branch name, update this test to assert success instead")
	}
	if !strings.Contains(err.Error(), "not a valid ref") && !strings.Contains(err.Error(), "not a valid branch name") {
		t.Fatalf("got a different error than expected, re-triage: %v", err)
	}
}

func TestCleanRefusesDirty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "dirty", "")
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
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "ahead", "")
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
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(repo, "clean", ""); err != nil {
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
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "drifts", "")
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
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Clean(repo, "never-existed", false); err == nil {
		t.Fatal("want error for a name with no worktree on disk")
	}
}

func TestCleanLockedRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "locked", "")
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
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(repo, "mergeable", ""); err != nil {
		t.Fatal(err)
	}
	dest2, err := Create(repo, "ahead", "")
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

func TestCleanAllSkipsOrphanedEntries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
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
