package diff

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// golden commands (captured against a repo built by testdata/pr/build_repo.sh, python3 on PATH):
//   python3 pr_analyzer.py <repo> --base main --head feature [--json]
// The repo is rebuilt fresh per test (deterministic author/committer dates -> identical commit
// hashes every time) so the test never reads this checkout's own live git history.

func buildPRFixtureRepo(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "repo")
	script, err := filepath.Abs(filepath.Join("testdata", "pr", "build_repo.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build_repo.sh: %v\n%s", err, out)
	}
	return dest
}

func readPRGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pr", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

func TestRunPRMatchesGolden(t *testing.T) {
	repo := buildPRFixtureRepo(t)
	wantText := readPRGolden(t, "text.want")
	wantJSON := readPRGolden(t, "json.want.json")

	var stdout, stderr bytes.Buffer
	if code := RunPR([]string{repo, "--base", "main", "--head", "feature"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != wantText {
		t.Errorf("text mismatch:\n--- got ---\n%s\n--- want ---\n%s", stdout.String(), wantText)
	}

	stdout.Reset()
	if code := RunPR([]string{repo, "--base", "main", "--head", "feature", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != wantJSON {
		t.Errorf("json mismatch:\n--- got ---\n%s\n--- want ---\n%s", stdout.String(), wantJSON)
	}
}

func TestRunPRNotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := RunPR([]string{dir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestRunPRBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunPR([]string{"--nope"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit code = 0 for a bad flag, want nonzero")
	}
}
