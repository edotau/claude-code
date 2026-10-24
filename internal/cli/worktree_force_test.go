package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// forceFixtureDirty makes the "feature" worktree dirty (untracked file) and one commit ahead of main.
func forceFixtureDirty(t *testing.T) (repo, wt string) {
	t.Helper()
	repo = mergeFixtureCLISetup(t)
	wt = mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, wt
}

func forceBranchList(t *testing.T, repo, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "branch", "--list", branch).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCmdWorktreeCleanForceRefusedWithoutFlag(t *testing.T) {
	repo, wt := forceFixtureDirty(t)
	code, _, _ := captureOutput(t, func() int { return cmdWorktree([]string{"clean", "feature"}) })
	if code == 0 {
		t.Fatal("plain clean of a dirty+unmerged worktree must fail")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must survive a refused clean: %v", err)
	}
	if forceBranchList(t, repo, "worktree-feature") == "" {
		t.Fatal("branch must survive a refused clean")
	}
}

func TestCmdWorktreeCleanForceDiscardsDirtyUnmerged(t *testing.T) {
	repo, wt := forceFixtureDirty(t)
	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"clean", "--force", "feature"}) })
	if code != 0 {
		t.Fatalf("clean --force: code=%d stdout=%q stderr=%q", code, out, errb)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("worktree dir still exists after clean --force")
	}
	if got := forceBranchList(t, repo, "worktree-feature"); got != "" {
		t.Fatalf("branch should be deleted with -D, branch --list = %q", got)
	}
	// summary names the discarded work: branch, uncommitted file, and unmerged commit
	for _, want := range []string{"discarding:", "uncommitted files: 1", "unmerged commits: 1 on worktree-feature"} {
		if !strings.Contains(out+errb, want) {
			t.Errorf("summary %q lacks %q", out+errb, want)
		}
	}
}

func TestCmdWorktreeCleanForceNeverTouchesMainCheckout(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	for _, name := range []string{"..", "../" + filepath.Base(repo), repo, "."} {
		code, _, _ := captureOutput(t, func() int { return cmdWorktree([]string{"clean", "--force", name}) })
		if code == 0 {
			t.Errorf("clean --force %q must fail", name)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatalf("main checkout damaged: %v", err)
	}
	if forceBranchList(t, repo, "worktree-feature") == "" {
		t.Fatal("unrelated branch lost")
	}
}
