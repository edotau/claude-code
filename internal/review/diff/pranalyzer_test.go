package diff

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
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
	if want := "Error: " + dir + " is not a git repository\n"; code != 1 || stdout.Len() != 0 || stderr.String() != want {
		t.Fatalf("RunPR = %d, stdout %q, stderr %q; want 1, empty, %q", code, &stdout, &stderr, want)
	}
}

func TestResolveGitRepoValidatesRepository(t *testing.T) {
	t.Run("empty dotgit is not a repository", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		got, ok := resolveGitRepo([]string{dir}, &stderr)
		if ok || got != "" {
			t.Fatalf("fake repository resolved to %q, %v", got, ok)
		}
		if stderr.Len() == 0 {
			t.Fatal("missing invalid-repository diagnostic")
		}
	})
	t.Run("subdirectory resolves to repository root", func(t *testing.T) {
		repo := hosttest.Repo(t)
		want, err := filepath.EvalSymlinks(repo)
		if err != nil {
			t.Fatal(err)
		}
		subdir := filepath.Join(repo, "nested")
		if err := os.Mkdir(subdir, 0o755); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		got, ok := resolveGitRepo([]string{subdir}, &stderr)
		if !ok || got != want {
			t.Fatalf("subdirectory resolved to %q, %v, stderr=%q; want %q", got, ok, &stderr, want)
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected diagnostic: %q", &stderr)
		}
	})
}

func TestWriteOutputAtomic(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sub", "report.txt")
	var so, se bytes.Buffer
	if code := writeOutput("first", false, out, "Results", &so, &se); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, se.String())
	}
	if code := writeOutput("second", false, out, "Results", &so, &se); code != 0 {
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
	if code := writeOutput("x", false, bad, "Results", &so, &se); code != 1 {
		t.Fatalf("want code 1, got %d", code)
	}
	if ents, _ := os.ReadDir(bad); len(ents) != 1 {
		t.Fatalf("failed write left debris: %v", ents)
	}
}

func commitFile(t *testing.T, repo, name, body, msg string) {
	t.Helper()
	hosttest.WriteRel(t, repo, name, body)
	hosttest.In(t, repo, "add", "--", name)
	hosttest.In(t, repo, "commit", "-m", msg)
}

// Commits only on base must not count as PR commits or draw commit-message findings.
func TestAnalyzePRCommitsExcludeBaseOnly(t *testing.T) {
	repo := hosttest.Repo(t)
	hosttest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "a.go", "package a\n", "feat: add a")
	hosttest.In(t, repo, "checkout", "main")
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
	repo := hosttest.Repo(t)
	hosttest.In(t, repo, "branch", "feature")
	hosttest.WriteRel(t, repo, "staged.go", "package s\n")
	hosttest.In(t, repo, "add", "--", "staged.go")
	rep, err := AnalyzePR(repo, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "no_changes" {
		t.Fatalf("status %q files %v, want no_changes", rep.Status, rep.Files)
	}
}

// A bad base or head ref is an error, never a clean "no_changes" nor an analysis of the staged index.
func TestAnalyzePRBadRefIsError(t *testing.T) {
	for _, tc := range []struct {
		name, base, head string
		staged           bool
	}{
		{"bad base, nothing staged", "nosuchref", "HEAD", false},
		{"bad head, staged edit", "main", "nosuchbranch", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := hosttest.Repo(t)
			hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
			if tc.staged {
				hosttest.WriteRel(t, repo, "staged.go", "package s\n")
				hosttest.In(t, repo, "add", "--", "staged.go")
			}
			if rep, err := AnalyzePR(repo, tc.base, tc.head); err == nil {
				t.Fatalf("AnalyzePR(%s, %s) = status %q, nil error; want a ref error", tc.base, tc.head, rep.Status)
			}
			var stdout, stderr bytes.Buffer
			if code := RunPR([]string{repo, "--base", tc.base, "--head", tc.head}, &stdout, &stderr); code != 1 || stderr.Len() == 0 {
				t.Errorf("RunPR exit = %d, stderr %q; want 1 with the ref error", code, &stderr)
			}
		})
	}
}

// With no merge base, the file list and per-file diff must both come from the two-dot fallback.
func TestFileDiffSharesTwoDotFallback(t *testing.T) {
	repo := hosttest.Repo(t)
	hosttest.In(t, repo, "checkout", "--orphan", "orphan")
	commitFile(t, repo, "o.go", "package o\n", "feat: orphan")
	files, err := changedFiles(repo, "main", "orphan")
	if err != nil || len(files) != 1 || files[0].Path != "o.go" {
		t.Fatalf("files %v err %v, want [o.go]", files, err)
	}
	if d, err := patchFor(repo, "o.go", "main", "orphan"); err != nil || d == "" {
		t.Fatalf("patchFor = %q, %v for a file changedFiles listed", d, err)
	}
}

// Non-ASCII names must arrive unquoted, so they match as pathspecs.
func TestChangedFilesNonASCIIPath(t *testing.T) {
	repo := hosttest.Repo(t)
	hosttest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "caf\u00e9.go", "package c\n", "feat: cafe")
	files, err := changedFiles(repo, "main", "feature")
	if err != nil || len(files) != 1 || files[0].Path != "caf\u00e9.go" {
		t.Fatalf("files %v err %v, want [café.go]", files, err)
	}
	if d, err := patchFor(repo, files[0].Path, "main", "feature"); err != nil || d == "" {
		t.Fatalf("patchFor = %q, %v for non-ASCII path", d, err)
	}
}

func failPatchGit(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = \"--\" ]; then\n    echo patch-read-failed >&2\n    exit 42\n  fi\ndone\n" + fmt.Sprintf("exec %q \"$@\"\n", git)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAnalyzePRPropagatesPatchFailure(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "app.go", "package app\n", "feat: app")
	failPatchGit(t)
	files, err := changedFiles(repo, "main", "feature")
	if err != nil || len(files) != 1 {
		t.Fatalf("file listing must succeed before patch failure: %v, %v", files, err)
	}
	if _, err := AnalyzePR(repo, "main", "feature"); err == nil || !strings.Contains(err.Error(), "app.go") || !strings.Contains(err.Error(), "42") {
		t.Fatalf("AnalyzePR error = %v, want app.go patch failure (exit 42)", err)
	}
}

func TestAnalyzePRFailsClosedOnGitLogError(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "app.go", "package app\n", "feat: app")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = \"log\" ]; then\n    echo log-failed >&2\n    exit 43\n  fi\ndone\n" + fmt.Sprintf("exec %q \"$@\"\n", git)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := AnalyzePR(repo, "main", "feature"); err == nil || !strings.Contains(err.Error(), "git log") {
		t.Fatalf("AnalyzePR error = %v, want a git log failure", err)
	}
}
