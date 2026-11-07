package diff

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest/gittest"
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

func TestWriteOutputAtomic(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sub", "report.txt")
	var so, se bytes.Buffer
	if code := writeOutput("first", out, &so, &se); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, se.String())
	}
	if code := writeOutput("second", out, &so, &se); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, se.String())
	}
	if got, _ := os.ReadFile(out); string(got) != "second" {
		t.Fatalf("report = %q", got)
	}
	if ents, _ := os.ReadDir(filepath.Dir(out)); len(ents) != 1 {
		t.Fatalf("leftover temp files: %v", ents)
	}
	// a failing write (destination is a directory) reports an error and leaves the directory intact
	bad := filepath.Join(dir, "sub")
	if code := writeOutput("x", bad, &so, &se); code != 1 {
		t.Fatalf("want code 1, got %d", code)
	}
	if ents, _ := os.ReadDir(bad); len(ents) != 1 {
		t.Fatalf("failed write left debris: %v", ents)
	}
}

func commitFile(t *testing.T, repo, name, body, msg string) {
	t.Helper()
	gittest.WriteRel(t, repo, name, body)
	gittest.In(t, repo, "add", "--", name)
	gittest.In(t, repo, "commit", "-m", msg)
}

// Commits only on base must not count as PR commits or draw commit-message findings.
func TestAnalyzePRCommitsExcludeBaseOnly(t *testing.T) {
	repo := gittest.Repo(t)
	gittest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "a.go", "package a\n", "feat: add a")
	gittest.In(t, repo, "checkout", "main")
	commitFile(t, repo, "b.go", "package b\n", "bad base only")
	rep, err := AnalyzePR(repo, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Summary.Commits != 1 || len(rep.CommitIssues) != 0 {
		t.Fatalf("commits=%d issues=%v, want 1 and none", rep.Summary.Commits, rep.CommitIssues)
	}
}

// A branch with nothing ahead of base is no_changes even when the index holds staged edits.
func TestAnalyzePRIgnoresStagedIndex(t *testing.T) {
	repo := gittest.Repo(t)
	gittest.In(t, repo, "branch", "feature")
	gittest.WriteRel(t, repo, "staged.go", "package s\n")
	gittest.In(t, repo, "add", "--", "staged.go")
	rep, err := AnalyzePR(repo, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "no_changes" {
		t.Fatalf("status %q files %v, want no_changes", rep.Status, rep.Files)
	}
}

// A misspelled head surfaces the ref error instead of analyzing the staged index.
func TestAnalyzePRBadRefNotMaskedByStaged(t *testing.T) {
	repo := gittest.Repo(t)
	gittest.WriteRel(t, repo, "staged.go", "package s\n")
	gittest.In(t, repo, "add", "--", "staged.go")
	if _, err := AnalyzePR(repo, "main", "nosuchbranch"); err == nil {
		t.Fatal("want an error for an unknown head")
	}
}

// With no merge base, the file list and per-file diff must both come from the two-dot fallback.
func TestFileDiffSharesTwoDotFallback(t *testing.T) {
	repo := gittest.Repo(t)
	gittest.In(t, repo, "checkout", "--orphan", "orphan")
	commitFile(t, repo, "o.go", "package o\n", "feat: orphan")
	files, err := changedFiles(repo, "main", "orphan")
	if err != nil || len(files) != 1 || files[0].Path != "o.go" {
		t.Fatalf("files %v err %v, want [o.go]", files, err)
	}
	if d := fileDiff(repo, "o.go", "main", "orphan"); d == "" {
		t.Fatal("fileDiff empty for a file changedFiles listed")
	}
}

// Non-ASCII names must arrive unquoted, so they match as pathspecs.
func TestChangedFilesNonASCIIPath(t *testing.T) {
	repo := gittest.Repo(t)
	gittest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "caf\u00e9.go", "package c\n", "feat: cafe")
	files, err := changedFiles(repo, "main", "feature")
	if err != nil || len(files) != 1 || files[0].Path != "caf\u00e9.go" {
		t.Fatalf("files %v err %v, want [café.go]", files, err)
	}
	if d := fileDiff(repo, files[0].Path, "main", "feature"); d == "" {
		t.Fatal("fileDiff empty for non-ASCII path")
	}
}
