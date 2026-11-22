package proc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGitEnvStripsGitPrefixed(t *testing.T) {
	t.Setenv("GIT_DIR", "/somewhere/.git")
	t.Setenv("GIT_WORK_TREE", "/somewhere")
	t.Setenv("GITEXEC_TEST_KEEP", "keep-me")

	env := GitEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") {
			t.Fatalf("GitEnv() leaked a GIT_* var: %q", kv)
		}
	}
	found := false
	for _, kv := range env {
		if kv == "GITEXEC_TEST_KEEP=keep-me" {
			found = true
		}
	}
	if !found {
		t.Fatal("GitEnv() dropped a non-GIT_* var it should have kept")
	}
}

func TestGitEnvDoesNotMutateProcessEnviron(t *testing.T) {
	before := len(os.Environ())
	_ = GitEnv()
	after := len(os.Environ())
	if before != after {
		t.Fatalf("GitEnv() mutated the process environment: before=%d after=%d", before, after)
	}
}

func TestRunGitTrimsOnlyTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	out, err := RunGit(context.Background(), dir, "init", "-q")
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	if out != "" {
		t.Fatalf("git init output = %q, want empty", out)
	}

	out, err = RunGit(context.Background(), dir, "status", "--short")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if out != "" {
		t.Fatalf("git status output = %q, want empty on a fresh repo", out)
	}

	// echo through git's own machinery is unavailable; verify trim semantics directly via a command
	// whose output is predictable: `git --version` is single-line with no leading/trailing padding
	// other than the trailing newline the OS appends, which RunGit must strip.
	out, err = RunGit(context.Background(), "", "--version")
	if err != nil {
		t.Fatalf("git --version: %v", err)
	}
	if strings.HasSuffix(out, "\n") {
		t.Fatalf("RunGit left a trailing newline: %q", out)
	}
	if strings.TrimLeft(out, " ") != out {
		t.Fatalf("RunGit trimmed leading whitespace, want right-trim only: %q", out)
	}
}

func TestRunGitDirBecomesLeadingC(t *testing.T) {
	dir := t.TempDir()
	if _, err := RunGit(context.Background(), dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	// Create a file and check status by passing dir as the -C target, with no -C in args.
	if err := os.WriteFile(dir+"/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := RunGit(context.Background(), dir, "status", "--short")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.Contains(out, "f.txt") {
		t.Fatalf("RunGit(dir=%q) did not target dir via -C: out=%q", dir, out)
	}
}

func TestRunGitEmptyDirOmitsC(t *testing.T) {
	_, err := RunGit(context.Background(), "", "not-a-real-git-subcommand")
	if err == nil {
		t.Fatal("expected an error for an invalid git subcommand")
	}
	if strings.Contains(err.Error(), "-C") {
		t.Fatalf("dir == \"\" must not inject -C: %q", err)
	}
}

func TestRunGitErrorWrapsArgvAndStderr(t *testing.T) {
	_, err := RunGit(context.Background(), "", "not-a-real-git-subcommand")
	if err == nil {
		t.Fatal("expected an error for an invalid git subcommand")
	}
	msg := err.Error()
	if !strings.Contains(msg, "git not-a-real-git-subcommand") {
		t.Fatalf("error does not include the full argv: %q", msg)
	}
}

func TestRunGitStripsGitPrefixedFromSubprocessEnv(t *testing.T) {
	t.Setenv("GIT_TEST_X", "1")
	dir := t.TempDir()
	if _, err := RunGit(context.Background(), dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	out, err := RunGit(context.Background(), dir, "-c", "alias.e=!env", "e")
	if err != nil {
		t.Fatalf("git e: %v", err)
	}
	if strings.Contains(out, "GIT_TEST_X") {
		t.Fatalf("RunGit leaked GIT_TEST_X into the subprocess: %q", out)
	}
}

func TestRunGitInheritPassesGitPrefixedThrough(t *testing.T) {
	t.Setenv("GIT_TEST_X", "1")
	dir := t.TempDir()
	if _, err := RunGit(context.Background(), dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	out, err := RunGitInherit(context.Background(), dir, "-c", "alias.e=!env", "e")
	if err != nil {
		t.Fatalf("git e: %v", err)
	}
	if !strings.Contains(out, "GIT_TEST_X=1") {
		t.Fatalf("RunGitInherit did not pass GIT_TEST_X through: %q", out)
	}
}

func TestRunGitDeadlineWithInheritedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git's ! alias needs a POSIX sh with sleep and job control")
	}
	orig := ExecWaitDelay
	ExecWaitDelay = 100 * time.Millisecond
	t.Cleanup(func() { ExecWaitDelay = orig })
	dir := t.TempDir()
	const deadline = 700 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	// The bounded child retains captured stdout after the context kills git.
	start := time.Now()
	pidFile := dir + "/child.pid"
	t.Cleanup(func() { killPidFileChild(t, pidFile) })
	_, err := RunGit(ctx, dir, "-c", "alias.hold=!sleep 6 & echo $! > child.pid; echo ready > ready; wait", "hold")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("RunGit succeeded despite the context deadline")
	}
	if marker, err := os.ReadFile(dir + "/ready"); err != nil || string(marker) != "ready\n" {
		t.Fatalf("git alias did not reach its wait: marker=%q, err=%v", marker, err)
	}
	if elapsed < ExecWaitDelay || elapsed > deadline+ExecWaitDelay+2*time.Second {
		t.Fatalf("RunGit with inherited stdout returned after %v; want %v–%v (WaitDelay must bound descendant output)",
			elapsed, ExecWaitDelay, deadline+ExecWaitDelay+2*time.Second)
	}
}

func killPidFileChild(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func TestRunGitHonorsContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	_, err := RunGit(ctx, "", "--version")
	if err == nil {
		t.Fatal("expected an error from an already-expired context")
	}
}

func BenchmarkRunGit(b *testing.B) {
	dir := b.TempDir()
	gitDir := filepath.Join(dir, ".git")
	for _, name := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(gitDir, name), 0o700); err != nil {
			b.Fatal(err)
		}
	}
	for name, data := range map[string]string{
		"HEAD":   "ref: refs/heads/bench\n",
		"config": "[core]\nrepositoryformatversion = 0\nbare = false\n",
	} {
		if err := os.WriteFile(filepath.Join(gitDir, name), []byte(data), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	b.Setenv("HOME", b.TempDir())
	b.Setenv("XDG_CONFIG_HOME", b.TempDir())
	b.Setenv("GIT_DIR", filepath.Join(dir, "must-be-stripped"))
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		got, err := RunGit(ctx, dir, "rev-parse", "--is-inside-work-tree")
		if err != nil || got != "true" {
			b.Fatalf("RunGit = %q, %v; want true", got, err)
		}
	}
}

func BenchmarkStatusEntries(b *testing.B) {
	const record = " M src/edited.go\x00R  src/new.go\x00src/old.go\x00?? untracked file.txt\x00"
	out := strings.Repeat(record, 100)
	b.SetBytes(int64(len(out)))
	b.ReportAllocs()
	for b.Loop() {
		got := StatusEntries(out)
		if len(got) != 300 || got[1].From != "src/old.go" || got[299].Path != "untracked file.txt" {
			b.Fatal("status parser lost an entry, rename source, or space in a path")
		}
	}
}
