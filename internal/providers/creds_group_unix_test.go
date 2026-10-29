//go:build unix

package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A timed-out auth command takes its whole pipeline with it: a backgrounded child must not outlive the call.
func TestRunAuthCommandTimeoutKillsDescendants(t *testing.T) {
	old := authCommandTimeout
	authCommandTimeout = 300 * time.Millisecond
	t.Cleanup(func() { authCommandTimeout = old })
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	_, err := runAuthCommand(context.Background(), &Provider{Name: "slow"}, "sleep 60 & echo $! > "+pidfile+"; sleep 60")
	if err == nil {
		t.Fatal("a command that outlives the timeout must fail")
	}
	raw, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("the descendant never started: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	for range 40 {
		if kerr := syscall.Kill(pid, 0); errors.Is(kerr, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("descendant %d still running after the auth command timed out", pid)
}

// A shell that exits before its background child must not leave the child running.
func TestRunAuthCommandReapsChildAfterShellExits(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	v, err := runAuthCommand(context.Background(), &Provider{Name: "early"},
		"(sleep 60 >/dev/null 2>&1 & echo $! > "+pidfile+"); echo tok")
	if err != nil || v != "tok" {
		t.Fatalf("runAuthCommand = %q, %v; want tok", v, err)
	}
	raw, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("the child never started: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	for range 40 {
		if kerr := syscall.Kill(pid, 0); errors.Is(kerr, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("child %d still running after its shell exited", pid)
}
