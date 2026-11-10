package worktree

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolveRepoBoundsDiscoverySubprocess(t *testing.T) {
	orig := probeTimeout
	probeTimeout = time.Second
	t.Cleanup(func() { probeTimeout = orig })
	regressionFakeGit(t, fmt.Sprintf("exec /bin/sleep %d\n", int((probeTimeout+4*time.Second)/time.Second)))
	start := time.Now()
	_, err := ResolveRepo(t.TempDir())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hung discovery must fail")
	}
	if elapsed >= probeTimeout+3*time.Second {
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
