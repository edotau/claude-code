package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stubProcesses swaps processesIn/parentOf for the duration of the test and restores the platform
// originals on cleanup, so a stub in one test never leaks into the next.
func stubProcesses(t *testing.T, in func(dir string) ([]Proc, error)) {
	t.Helper()
	origIn, origParent, origSettle := processesIn, parentOf, inUseSettle
	processesIn = in
	parentOf = func(pid int) (int, error) { return 0, nil } // no ancestry beyond the stubbed pid itself
	inUseSettle = 0                                         // stubbed processes never exit; refuse on the first look
	t.Cleanup(func() { processesIn, parentOf, inUseSettle = origIn, origParent, origSettle })
}

// A process that exits within the settle window (git's background auto-gc) does not block removal.
func TestInUseByWaitsOutTransientProcess(t *testing.T) {
	dest := t.TempDir()
	calls := 0
	stubProcesses(t, func(dir string) ([]Proc, error) {
		calls++
		if calls <= 2 {
			return []Proc{{PID: 999999, Comm: "git"}}, nil
		}
		return nil, nil
	})
	inUseSettle = time.Second
	reason, err := InUseBy(dest)
	if err != nil || reason != "" {
		t.Fatalf("InUseBy = %q, %v; want clear once the transient process exits", reason, err)
	}
	if calls < 3 {
		t.Errorf("checked %d times, want a re-check after the first busy look", calls)
	}
}

func TestInUseByRefusesWhenStubbedProcessIsInside(t *testing.T) {
	dest := t.TempDir()
	stubProcesses(t, func(dir string) ([]Proc, error) {
		return []Proc{{PID: 999999, Comm: "fakeproc"}}, nil
	})
	reason, err := InUseBy(dest)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason == "" {
		t.Fatal("want a refusal reason for a stubbed process inside dest")
	}
	if !strings.Contains(reason, "999999") || !strings.Contains(reason, "fakeproc") {
		t.Errorf("reason = %q, want it to name the pid and comm", reason)
	}
}

func TestInUseByClearWhenNoProcess(t *testing.T) {
	dest := t.TempDir()
	stubProcesses(t, func(dir string) ([]Proc, error) { return nil, nil })
	reason, err := InUseBy(dest)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want clear", reason)
	}
}

func TestInUseByExcludesCallerAndAncestors(t *testing.T) {
	dest := t.TempDir()
	caller := os.Getpid()
	stubProcesses(t, func(dir string) ([]Proc, error) {
		return []Proc{{PID: caller, Comm: "self"}}, nil
	})
	reason, err := InUseBy(dest)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want the caller's own pid excluded", reason)
	}
}

// TestWithinPrefixNotConfused is the regression test for a naive strings.HasPrefix: /a/foo must not
// match a process whose cwd is /a/foobar.
func TestWithinPrefixNotConfused(t *testing.T) {
	if within("/a/foobar", "/a/foo") {
		t.Fatal("within(/a/foobar, /a/foo) = true, want false — foobar is a sibling, not a child")
	}
	if !within("/a/foo", "/a/foo") {
		t.Fatal("within(/a/foo, /a/foo) = false, want true — equal paths are inside")
	}
	if !within("/a/foo/bar", "/a/foo") {
		t.Fatal("within(/a/foo/bar, /a/foo) = false, want true — a real child")
	}
	if !within("/a/foo/", "/a/foo") {
		t.Fatal("within with a trailing separator should still match")
	}
}

func TestInUseBySymlinkedPathMatched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated privileges on windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real) // darwin's TempDir is itself under the /var -> /private/var symlink
	if err != nil {
		t.Fatal(err)
	}
	stubProcesses(t, func(dir string) ([]Proc, error) {
		// production processesIn implementations report the resolved cwd; dir passed in is already resolved.
		if within(resolved, dir) {
			return []Proc{{PID: 999999, Comm: "throughlink"}}, nil
		}
		return nil, nil
	})
	// InUseBy is called with the symlink path; it must resolve it before comparing/scanning.
	reason, err := InUseBy(link)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason == "" {
		t.Fatal("want a refusal: a process inside the symlink target must be detected via the symlink path")
	}
}

func TestInUseByCwdInsideRefuses(t *testing.T) {
	dest := t.TempDir()
	stubProcesses(t, func(dir string) ([]Proc, error) { return nil, nil })
	t.Chdir(dest)
	reason, err := InUseBy(dest)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason == "" {
		t.Fatal("want a refusal when the caller's own cwd is inside dest")
	}
	if !strings.Contains(reason, "cd out") {
		t.Errorf("reason = %q, want it to mention cd-ing out", reason)
	}
}

func TestInUseByCwdOutsideClear(t *testing.T) {
	dest := t.TempDir()
	outside := t.TempDir()
	stubProcesses(t, func(dir string) ([]Proc, error) { return nil, nil })
	t.Chdir(outside)
	reason, err := InUseBy(dest)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want clear when cwd is outside dest", reason)
	}
}

// TestInUseByRealProcess is the one real-process integration test, Linux/macOS only (CI gates on
// both per decisionLog): a genuine child process with its cwd inside a worktree must be detected via
// the platform's own /proc or lsof implementation (no stub), and the refusal must clear once it exits.
func TestInUseByRealProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("real-process detection is only implemented for linux and darwin")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	origSettle := inUseSettle
	inUseSettle = 0 // the sleep never exits on its own; refuse on the first look
	t.Cleanup(func() { inUseSettle = origSettle })
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "busy", "")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sleep", "30")
	cmd.Dir = dest
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// Poll briefly: the child's /proc entry (or lsof's view) can lag the Start() call by a beat.
	deadline := time.Now().Add(5 * time.Second)
	var reason string
	for time.Now().Before(deadline) {
		reason, err = InUseBy(dest)
		if err != nil {
			t.Fatalf("InUseBy: %v", err)
		}
		if reason != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if reason == "" {
		t.Fatal("want a refusal while the real sleep process has its cwd inside dest")
	}
	if !strings.Contains(reason, "in use by pid") {
		t.Errorf("reason = %q, want the pid-based in-use message", reason)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill sleep: %v", err)
	}
	_ = cmd.Wait()

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reason, err = InUseBy(dest)
		if err != nil {
			t.Fatalf("InUseBy after kill: %v", err)
		}
		if reason == "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want clear after the process was killed", reason)
	}
}
