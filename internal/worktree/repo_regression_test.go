package worktree

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolveRepoBoundsDiscoverySubprocess(t *testing.T) {
	regressionFakeGit(t, "exec /bin/sleep 34\n")
	start := time.Now()
	_, err := ResolveRepo(t.TempDir())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hung discovery must fail")
	}
	if elapsed >= 33*time.Second {
		t.Fatalf("git discovery ignored its deadline: %s", elapsed)
	}
}

func TestResolveWorktreePropagatesLstatError(t *testing.T) {
	repo := testRepo(t)
	repo.Root = filepath.Join(t.TempDir(), "not-a-directory")
	regressionWriteFile(t, repo.Root, "file\n")
	dest, err := ResolveWorktree(repo, "feature")
	if !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "inspect") || dest != "" {
		t.Fatalf("Lstat failure bypassed validation: dest=%q err=%v", dest, err)
	}
}
