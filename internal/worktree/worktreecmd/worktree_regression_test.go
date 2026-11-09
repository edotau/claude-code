package worktreecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestCmdWorktreeMergeDryRunResumeRequiresOperation(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	before := cliHeadOf(t, repo)
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"merge", "--dry-run", "--resume", "feature"})
	})
	if code != 1 || !strings.Contains(stderr, "no in-progress --resolve-agent operation") {
		t.Fatalf("dry-run resume without operation: code=%d stdout=%q stderr=%q", code, out, stderr)
	}
	if strings.Contains(out, "clean") {
		t.Fatalf("invalid resume printed a clean plan: %q", out)
	}
	if cliHeadOf(t, repo) != before {
		t.Fatal("dry-run resume changed main HEAD")
	}
	if _, err := os.Stat(mergeFixtureCLIWorktreePath(t, repo)); err != nil {
		t.Fatalf("dry-run resume removed worktree: %v", err)
	}
}

func TestCmdWorktreeCleanForceBranchDeletionFailure(t *testing.T) {
	repo, wt := forceFixtureDirty(t)
	lock := filepath.Join(repo, ".git", "refs", "heads", "worktree-feature.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"clean", "--force", "feature"})
	})
	if code != 1 || !strings.Contains(stderr, "branch not deleted") || !strings.Contains(stderr, "worktree-feature.lock") {
		t.Fatalf("locked branch deletion: code=%d stdout=%q stderr=%q", code, out, stderr)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed despite branch deletion failure: %v", err)
	}
	if forceBranchList(t, repo, "worktree-feature") == "" {
		t.Fatal("locked branch should remain after failed deletion")
	}
}

func TestCmdWorktreeCreateStdoutFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	unwritable, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unwritable.Close(); err != nil {
			t.Error(err)
		}
	})
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		stdout := os.Stdout
		os.Stdout = unwritable
		defer func() { os.Stdout = stdout }()
		return Run([]string{"create", "feature"})
	})
	dest := mergeFixtureCLIWorktreePath(t, repo)
	if code != 1 || !strings.Contains(stderr, "could not print its path") || !strings.Contains(stderr, dest) {
		t.Fatalf("create with unwritable stdout: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("created worktree should remain at reported path %s: %v", dest, err)
	}
}
