package diff

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/proc"
)

// RunGate ports code-review-gate.sh, an advisory hook with no Python CLI of its own (always exit 0).
// No golden capture is possible (nothing to invoke); these tests build a throwaway git repo in-process
// and assert the gate's own documented contract: always exit 0, and the section markers the shell
// script prints ("--- code-review gate ---", "[simplicity]", "[surgical]").

func newGateRepo(t *testing.T) string {
	t.Helper()
	return hosttest.Repo(t)
}

// gitAddInherited stages with the test's own GIT_INDEX_FILE, which hosttest.In strips.
func gitAddInherited(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := proc.RunGitInherit(t.Context(), dir, append([]string{"add"}, args...)...); err != nil {
		t.Fatal(err)
	}
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
	hosttest.In(t, dir, "add", "app.js")
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

// TestGateStagedFilesHonorsInheritedGitIndexFile proves stagedFiles inherits GIT_INDEX_FILE
// (via RunGitInherit) instead of stripping it (RunGit would): git itself sets GIT_INDEX_FILE to a
// temp index while running a hook, and the gate must see what's staged in THAT index, not the real one.
func TestGateStagedFilesHonorsInheritedGitIndexFile(t *testing.T) {
	dir := newGateRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	altIndex := filepath.Join(t.TempDir(), "alt-index")
	t.Setenv("GIT_INDEX_FILE", altIndex)
	gitAddInherited(t, dir, "a.txt")

	files, err := stagedFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if f == "a.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("stagedFiles(dir) with GIT_INDEX_FILE=%q did not include a.txt staged in the alt index: %v", altIndex, files)
	}
}

// An unknown flag neither blocks (exit 0) nor skips the review: the --repo beside it is still honored.
func TestRunGateAlwaysExitsZeroOnBadArgs(t *testing.T) {
	dir := newGateRepo(t)
	hosttest.WriteRel(t, dir, "app.js", "function run() {\n  console.log('hi')\n}\n")
	hosttest.In(t, dir, "add", "--", "app.js")
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	code := RunGate([]string{"--this-flag-does-not-exist", "--repo", dir}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "[surgical] diff_surgeon (staged):") {
		t.Fatalf("exit code = %d, stdout %q; want 0 and the staged review of %s", code, &stdout, dir)
	}
}

// RunGate --repo must inspect that repo, not the cwd: a staged file in dir shows up when run from elsewhere.
func TestRunGateRepoFlagFromOtherCwd(t *testing.T) {
	dir := newGateRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("function run() {\n  console.log('hi')\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, dir, "add", "app.js")
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
func TestChangedSourceFilesFromSubdir(t *testing.T) {
	root := newGateRepo(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	hosttest.In(t, root, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, root, "add", "sub/a.go")
	hosttest.In(t, root, "commit", "-q", "-m", "add")

	got, err := changedSourceFiles(sub, "main", "feature")
	if err != nil || len(got) != 1 || got[0] != "a.go" {
		t.Errorf("changedSourceFiles(sub) = %v, %v, want [a.go]", got, err)
	}
}

func TestRunGateUsesIndexContents(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.WriteRel(t, repo, "app.py", "def run(x):\n"+strings.Repeat("    x += 1\n", 150))
	hosttest.In(t, repo, "add", "--", "app.py")
	var want, stderr bytes.Buffer
	if code := RunGate([]string{"--repo", repo}, &want, &stderr); code != 0 || !strings.Contains(want.String(), "[WARN]") {
		t.Fatalf("staged complexity warning missing: exit %d, %s, %s", code, &want, &stderr)
	}
	hosttest.WriteRel(t, repo, "app.py", "def run(x):\n    return x\n")
	var got bytes.Buffer
	if code := RunGate([]string{"--repo", repo}, &got, &stderr); code != 0 || got.String() != want.String() {
		t.Fatalf("unstaged edit changed gate: exit %d, got %s; want %s", code, &got, &want)
	}
}

func TestGateComplexityIncludesGo(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.WriteRel(t, repo, "app.go", "package app\n")
	hosttest.In(t, repo, "add", "--", "app.go")
	want := []string{filepath.Join(repo, "app.go")}
	if got, err := stagedSourceFiles(repo); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("staged complexity files = %v, %v, want %v", got, err, want)
	}
}

func TestRunGateHonorsAlternateIndexBlob(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.WriteRel(t, repo, "app.py", "def run(x):\n    return x\n")
	hosttest.In(t, repo, "add", "--", "app.py")
	t.Setenv("GIT_INDEX_FILE", filepath.Join(repo, ".git", "index"))
	var stdout, stderr bytes.Buffer
	if code := RunGate([]string{"--repo", repo}, &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), "[WARN]") {
		t.Fatalf("simple default index: exit %d, stdout %s, stderr %s", code, &stdout, &stderr)
	}
	content := "\ndef run(x):\n" + strings.Repeat("    x += 1\n", 150) + "\n\t \n"
	hosttest.WriteRel(t, repo, "app.py", content)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "alternate-index"))
	gitAddInherited(t, repo, "--", "app.py")
	hosttest.WriteRel(t, repo, "app.py", "def run(x):\n    return x\n")
	stdout.Reset()
	stderr.Reset()
	if code := RunGate([]string{"--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("alternate index gate: exit %d, stderr %s", code, &stderr)
	}
	if out := stdout.String(); !strings.Contains(out, "[WARN]") || strings.Contains(out, "detector failed") {
		t.Fatalf("missing complexity warning for alternate-index blob:\n%s", out)
	}
	got, err := stagedBlob(repo)(filepath.Join(repo, "app.py"))
	if err != nil || !bytes.Equal(got, []byte(content)) {
		t.Fatalf("alternate-index blob = %q, %v; want unchanged bytes %q", got, err, content)
	}
}

func TestRunGateReviewsLocallyRemovedStagedFile(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.WriteRel(t, repo, "app.py", "def run(x):\n"+strings.Repeat("    x += 1\n", 150))
	hosttest.In(t, repo, "add", "--", "app.py")
	if err := os.Remove(filepath.Join(repo, "app.py")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := RunGate([]string{"--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("gate exit = %d, stderr = %q; want advisory exit 0", code, &stderr)
	}
	out := stdout.String()
	if !strings.Contains(out, "[simplicity] complexity_checker (medium):") || !strings.Contains(out, "[WARN]") {
		t.Fatalf("missing staged complexity warning after local removal:\n%s", out)
	}
	if strings.Contains(out, "detector failed") {
		t.Fatalf("staged file must be analyzed, not reported as a detector failure:\n%s", out)
	}
}
