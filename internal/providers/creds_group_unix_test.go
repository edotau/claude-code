//go:build unix

package providers

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
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
		if gone(t, pid) {
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
		if gone(t, pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("child %d still running after its shell exited", pid)
}

// gone reports whether pid has exited; a zombie counts (a slow-reaping init must not fail the test), and an
// inspection failure fails the test rather than passing it.
func gone(t *testing.T, pid int) bool {
	t.Helper()
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	st := strings.TrimSpace(string(out))
	if err != nil && st == "" {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			t.Fatalf("cannot inspect pid %d: %v", pid, err)
		}
		return false
	}
	return strings.HasPrefix(st, "Z")
}

// A background child that keeps the shell's stdout open must not delay or fail a token the shell already printed.
func TestRunAuthCommandChildHoldingStdoutDoesNotBlock(t *testing.T) {
	start := time.Now()
	v, err := runAuthCommand(context.Background(), &Provider{Name: "held"}, "sleep 60 & echo tok")
	if err != nil || v != "tok" {
		t.Fatalf("runAuthCommand = %q, %v; want tok", v, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s: a child holding stdout open must not wait out WaitDelay", d)
	}
}

// A child that escapes the group with setsid and keeps stdout open must not hang the auth command.
func TestRunAuthCommandEscapedChildDoesNotHang(t *testing.T) {
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid not on PATH")
	}
	old := authDrainLimit
	authDrainLimit = 300 * time.Millisecond
	t.Cleanup(func() { authDrainLimit = old })
	pidfile := filepath.Join(t.TempDir(), "escaped.pid")
	start := time.Now()
	v, rerr := runAuthCommand(context.Background(), &Provider{Name: "escaped"},
		setsid+" sh -c 'echo $$ > "+pidfile+"; exec sleep 60' & while [ ! -s "+pidfile+" ]; do sleep 0.05; done; echo tok")
	if raw, err := os.ReadFile(pidfile); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	}
	if rerr != nil || v != "tok" {
		t.Fatalf("runAuthCommand = %q, %v; want tok", v, rerr)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s: draining must be bounded by authDrainLimit", d)
	}
}

// io.Copy into a capBuffer keeps at most max bytes, however much the helper writes.
func TestCapBufferBoundsIOCopy(t *testing.T) {
	c := &capBuffer{max: 1024}
	n, err := io.Copy(c, io.LimitReader(zeroReader{}, 1<<20))
	if err != nil || n != 1<<20 || len(c.Bytes()) != 1024 {
		t.Fatalf("copied %d (err %v), kept %d bytes; want all consumed and 1024 kept", n, err, len(c.Bytes()))
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// Oversized stdout is rejected, not truncated into something that passes token validation.
func TestRunAuthCommandRejectsOversizedStdout(t *testing.T) {
	_, err := runAuthCommand(context.Background(), &Provider{Name: "big"}, "echo tok; head -c 1100000 /dev/zero | tr '\\0' ' '; echo two")
	if err == nil {
		t.Fatal("an auth command printing over 1 MiB must fail")
	}
}

// The stderr tail survives verbose output, so the final network error still classifies the failure.
func TestRunAuthCommandKeepsStderrTail(t *testing.T) {
	_, err := runAuthCommand(context.Background(), &Provider{Name: "noisy"}, "head -c 100000 /dev/zero | tr '\\0' x >&2; echo >&2; echo 'dial tcp: connection refused' >&2; exit 1")
	if err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("err = %v, want a network classification from the stderr tail", err)
	}
}
