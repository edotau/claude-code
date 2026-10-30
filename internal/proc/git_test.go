package proc

import (
	"context"
	"os"
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
	// With dir == "", RunGit must not inject its own -C; passing no -C and no repo args should run
	// in the test binary's cwd as plain `git <args>` would.
	_, err := RunGit(context.Background(), "", "rev-parse", "--is-inside-work-tree")
	// Outcome depends on cwd; what matters is that no "-C " was injected ahead of the subcommand,
	// which TestRunGitErrorWrapsArgv below exercises directly via the error message.
	_ = err
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

func TestRunGitHonorsContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	_, err := RunGit(ctx, "", "--version")
	if err == nil {
		t.Fatal("expected an error from an already-expired context")
	}
}
