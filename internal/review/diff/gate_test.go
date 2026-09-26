package diff

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RunGate ports code-review-gate.sh, an advisory hook with no Python CLI of its own (always exit 0).
// No golden capture is possible (nothing to invoke); these tests build a throwaway git repo in-process
// and assert the gate's own documented contract: always exit 0, and the section markers the shell
// script prints ("--- code-review gate ---", "[simplicity]", "[surgical]").

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func newGateRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustRun(t, dir, "git", "init", "-q", "-b", "main")
	mustRun(t, dir, "git", "config", "user.name", "Test")
	mustRun(t, dir, "git", "config", "user.email", "test@example.com")
	return dir
}

func TestRunGateNoStagedFiles(t *testing.T) {
	dir := newGateRepo(t)
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	code := RunGate(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout.String() != "" {
		t.Errorf("expected no output with nothing staged, got %q", stdout.String())
	}
}

func TestRunGateStagedFiles(t *testing.T) {
	dir := newGateRepo(t)
	// A staged .js file gives the complexity detector something to run on, plus a diff to surgeon.
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("function run() {\n  console.log('hi')\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "git", "add", "app.js")
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := RunGate(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (gate is advisory-only, never blocks)", code)
	}
	out := stdout.String()
	for _, marker := range []string{
		"--- code-review gate (advisory) ---",
		"[simplicity] complexity_checker (medium):",
		"[surgical] diff_surgeon (staged):",
		"--- /code-review gate ---",
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("output missing marker %q; got:\n%s", marker, out)
		}
	}
}

func TestRunGateAlwaysExitsZeroOnBadArgs(t *testing.T) {
	dir := newGateRepo(t)
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	code := RunGate([]string{"--this-flag-does-not-exist"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 — RunGate must never block on bad input", code)
	}
}
