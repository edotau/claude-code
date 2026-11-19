package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestRequireBranchPreservesGitFailures(t *testing.T) {
	repo := testRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"cancelled", ctx, context.Canceled},
		{"git-missing", context.Background(), exec.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "git-missing" {
				t.Setenv("PATH", t.TempDir())
			}
			_, err := requireBranch(tc.ctx, repo.Main)
			if !errors.Is(err, tc.want) || errors.Is(err, errDetachedHead) {
				t.Fatalf("git infrastructure failure misreported as detached HEAD: %v; want %v", err, tc.want)
			}
			plan := &MergePlan{}
			stop, err := screenTargetBranch(tc.ctx, repo, plan, "main")
			if !errors.Is(err, tc.want) || stop || len(plan.Issues) != 0 {
				t.Fatalf("target screening hid infrastructure failure: stop=%v issues=%v err=%v", stop, plan.Issues, err)
			}
		})
	}
}

func TestRunGitCancellationBoundsInheritedPipes(t *testing.T) {
	orig := gitWaitDelay
	gitWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { gitWaitDelay = orig })
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	expired := filepath.Join(t.TempDir(), "expired")
	regressionFakeGit(t, fmt.Sprintf("(/bin/sleep 30 & echo $! > %q; wait; touch %q) &\nwait\n", pidFile, expired))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// cancel only once the fake git has recorded its child, so the pid file always exists
	go func() {
		for ; ; time.Sleep(10 * time.Millisecond) {
			if data, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(data), "\n") {
				break
			}
		}
		cancel()
	}()
	_, err := runGit(ctx, t.TempDir(), nil, "status")
	_, expiredErr := os.Stat(expired)
	regressionKillChild(t, pidFile)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("git call must fail after cancellation: err=%v ctx=%v", err, ctx.Err())
	}
	if expiredErr == nil {
		t.Fatal("cancelled git waited for the child holding its output pipes to exit")
	}
}

func regressionFakeGit(t *testing.T, body string) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func regressionKillChild(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		t.Error(err)
	}
}
