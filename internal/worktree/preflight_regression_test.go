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
	regressionFakeGit(t, fmt.Sprintf("/bin/sleep 10 &\necho $! > %q\nwait\n", pidFile))
	timeout := 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	_, err := runGit(ctx, t.TempDir(), nil, "status")
	elapsed := time.Since(start)
	regressionKillChild(t, pidFile)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("git call must fail after cancellation: err=%v ctx=%v", err, ctx.Err())
	}
	if elapsed >= timeout+gitWaitDelay+2*time.Second {
		t.Fatalf("cancelled git waited for inherited output pipes: %s", elapsed)
	}
}

func regressionFakeGit(t *testing.T, body string) {
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
