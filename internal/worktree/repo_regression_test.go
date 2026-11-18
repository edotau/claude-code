package worktree

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestResolveRepoBoundsDiscoverySubprocess(t *testing.T) {
	orig := probeTimeout
	probeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { probeTimeout = orig })
	regressionFakeGit(t, fmt.Sprintf("exec /bin/sleep %d\n", int(time.Minute/time.Second)))
	_, err := ResolveRepo(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("hung discovery: err=%v, want git killed at its deadline", err)
	}
}

func TestResolveWorktreePropagatesLstatError(t *testing.T) {
	t.Parallel()
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	repo := testRepo(t)
	repo.Root = filepath.Join(t.TempDir(), "not-a-directory")
	regressionWriteFile(t, repo.Root, "file\n")
	dest, err := resolveWorktreeBounded(repo, "feature")
	if !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "inspect") || dest != "" {
		t.Fatalf("Lstat failure bypassed validation: dest=%q err=%v", dest, err)
	}
}
