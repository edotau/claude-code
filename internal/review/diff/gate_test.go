package diff

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest/gittest"
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
	return gittest.Repo(t)
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

// TestGateStagedFilesHonorsInheritedGitIndexFile proves gateStagedFiles inherits GIT_INDEX_FILE
// (via RunGitInherit) instead of stripping it (RunGit would): git itself sets GIT_INDEX_FILE to a
// temp index while running a hook, and the gate must see what's staged in THAT index, not the real one.
func TestGateStagedFilesHonorsInheritedGitIndexFile(t *testing.T) {
	dir := newGateRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	altIndex := filepath.Join(t.TempDir(), "alt-index")
	t.Setenv("GIT_INDEX_FILE", altIndex)
	mustRun(t, dir, "git", "add", "a.txt")

	files := gateStagedFiles(dir)
	found := false
	for _, f := range files {
		if f == "a.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gateStagedFiles(dir) with GIT_INDEX_FILE=%q did not include a.txt staged in the alt index: %v", altIndex, files)
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

// RunGate --repo must inspect that repo, not the cwd: a staged file in dir shows up when run from elsewhere.
func TestRunGateRepoFlagFromOtherCwd(t *testing.T) {
	dir := newGateRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("function run() {\n  console.log('hi')\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "git", "add", "app.js")
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := RunGate([]string{"--repo", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out := stdout.String(); strings.Contains(out, "detector failed") || !strings.Contains(out, "[surgical]") {
		t.Errorf("gate --repo from another cwd misreported:\n%s", out)
	}
}

// From a repo subdirectory git prints root-relative names; --relative keeps repo+"/"+line resolvable.
func TestGetChangedSourceFilesFromSubdir(t *testing.T) {
	root := newGateRepo(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, root, "git", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun(t, root, "git", "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, root, "git", "add", "sub/a.go")
	mustRun(t, root, "git", "commit", "-q", "-m", "add")

	got := getChangedSourceFiles(sub, "main", "feature")
	if len(got) != 1 || got[0] != sub+"/a.go" {
		t.Errorf("getChangedSourceFiles(sub) = %v, want [%s/a.go]", got, sub)
	}
}

// A bad base ref with nothing staged must be an error, not a clean "no_changes".
func TestAnalyzePRBadRefIsError(t *testing.T) {
	dir := newGateRepo(t)
	mustRun(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "init")
	if _, err := AnalyzePR(dir, "nosuchref", "HEAD"); err == nil {
		t.Fatal("AnalyzePR with a bad base ref returned nil error")
	}
	var stdout, stderr bytes.Buffer
	if code := RunPR([]string{dir, "--base", "nosuchref", "--head", "HEAD"}, &stdout, &stderr); code == 0 {
		t.Errorf("RunPR exit = 0 on bad ref; stderr=%q", stderr.String())
	}
}
