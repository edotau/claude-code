package diff

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestQualityCheckerUsesHeadContents(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "app.py", "def run():\n    return 1\n", "feat: add app")
	want, err := runQualityChecker(repo, "main", "feature")
	if err != nil || mInt(want, "files_analyzed") != 1 {
		t.Fatalf("head analysis = %v, %v; want one file", want, err)
	}
	hosttest.WriteRel(t, repo, "app.py", "def run():\n"+strings.Repeat("    print('debug')\n", 100))
	got, err := runQualityChecker(repo, "main", "feature")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("worktree changed head analysis: got %v, %v; want %v", got, err, want)
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
	if got := gateComplexityFiles(repo); !reflect.DeepEqual(got, want) {
		t.Fatalf("staged complexity files = %v, want %v", got, want)
	}
}

func TestChangedSourceFilesUnicode(t *testing.T) {
	repo := newGateRepo(t)
	hosttest.In(t, repo, "config", "core.quotePath", "true")
	hosttest.In(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "café.go", "package cafe\n", "feat: cafe")
	got, err := changedSourceFiles(repo, "main", "feature")
	if err != nil || !reflect.DeepEqual(got, []string{"café.go"}) {
		t.Fatalf("source files = %q, %v; want unquoted café.go", got, err)
	}
}

func TestBuildDiffResultQuoteSwapCountsBothLines(t *testing.T) {
	got := buildDiffResult([]*diffFileEntry{{file: "app.py", changes: []string{`-value = "hello"`, `+value = 'hello'`}}})
	if got.TotalChangeLine != 2 || got.NoiseLines != 2 || got.NoiseRatio != 1 || got.Verdict != "VERY_NOISY" {
		t.Fatalf("quote swap = %+v; want 2 changes, 2 noise lines, ratio 1, VERY_NOISY", got)
	}
}

func TestGenerateReportSingleFileQualitySummary(t *testing.T) {
	quality := map[string]any{
		"file": "app.py", "quality_score": 72.5,
		"smells":           []any{map[string]any{"type": "long_method"}, map[string]any{"type": "magic_number"}},
		"solid_violations": []any{map[string]any{"principle": "SRP"}},
	}
	got, err := generateReport("", "", "", map[string]any{"status": "no_changes"}, quality)
	want := &QualitySummary{FilesAnalyzed: 1, AverageScore: 72.5, TotalCodeSmells: 2, TotalSolidViolations: 1}
	if err != nil || !reflect.DeepEqual(got.QualitySummary, want) {
		t.Fatalf("single-file summary = %+v, %v; want %+v", got.QualitySummary, err, want)
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

type failedStdout struct{}

func (failedStdout) Write([]byte) (int, error) { return 0, errors.New("stdout unavailable") }

func TestReviewCommandsPropagateStdoutFailure(t *testing.T) {
	base := []string{
		filepath.Join("testdata", "report", "repo"),
		"--pr-analysis", filepath.Join("testdata", "report", "pr-fixture.json"),
		"--quality-analysis", filepath.Join("testdata", "report", "quality-fixture.json"),
	}
	repo := buildPRFixtureRepo(t)
	for _, tc := range []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
		args []string
	}{
		{"report text", RunReport, base},
		{"report json", RunReport, append(append([]string{}, base...), "--json")},
		{"pr text", RunPR, []string{repo, "--base", "main", "--head", "feature"}},
		{"diff json", RunDiff, []string{"--file", filepath.Join("testdata", "diff", "synthetic-noise.diff"), "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := tc.run(tc.args, failedStdout{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "stdout unavailable") {
				t.Fatalf("exit %d, stderr %q; want exit 1 with stdout failure", code, &stderr)
			}
		})
	}
}
