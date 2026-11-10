package diff

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/hosttest"
)

// golden commands (captured with python3 on PATH):
//   python3 diff_surgeon.py --file testdata/diff/<case>.diff [--json]
// real-commit-range.diff is `git diff c6965de..0977f77` saved once (hermetic — no live git read).

func readDiffGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "diff", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

func TestRunDiffMatchesGolden(t *testing.T) {
	cases := []struct {
		name string
		diff string
		want string
	}{
		{"synthetic text", "synthetic-noise.diff", "synthetic-noise.want"},
		{"synthetic json", "synthetic-noise.diff", "synthetic-noise.want.json"},
		{"real-commit text", "real-commit-range.diff", "real-commit-range.want"},
		{"real-commit json", "real-commit-range.diff", "real-commit-range.want.json"},
		{"empty text", "empty.diff", "empty.want"},
		{"empty json", "empty.diff", "empty.want.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jsonOut := filepath.Ext(tc.want) == ".json"
			args := []string{"--file", filepath.Join("testdata", "diff", tc.diff)}
			if jsonOut {
				args = append(args, "--json")
			}
			var stdout, stderr bytes.Buffer
			code := RunDiff(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			want := readDiffGolden(t, tc.want)
			if stdout.String() != want {
				t.Errorf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", stdout.String(), want)
			}
		})
	}
}

// Findings are advisory (exit 0); usage errors are not: a missing --file exits 1 with one clean line.
func TestRunDiffMissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunDiff([]string{"--file", "nosuch.diff"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "nosuch.diff not found") {
		t.Fatalf("exit %d stderr %q, want 1 and a not-found line", code, stderr.String())
	}
}

func TestRunDiffBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunDiff([]string{"--nope"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (argparse unrecognized arguments)", code)
	}
}

// A5: findings truncate by rune, so a multibyte line crossing the 60/120 cut stays valid UTF-8.
func TestAnalyzeDiffFileTruncatesOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 100)
	findings := analyzeDiffFile(&diffFileEntry{file: "x.go", changes: []string{"+// x" + long}})
	findings = append(findings, detectQuoteSwaps([]string{"-" + long + `"q"`, "+" + long + `'q'`})...)
	var comment, swap bool
	for _, f := range findings {
		if !utf8.ValidString(f.Line) {
			t.Errorf("%s finding is not valid UTF-8: %q", f.Category, f.Line)
		}
		comment = comment || f.Category == "comment-only"
		swap = swap || f.Category == "quote-style-swap"
	}
	if !comment || !swap {
		t.Fatalf("want comment-only and quote-style-swap findings, got %+v", findings)
	}
}

// A '#include' or '*p = 1' line in a C file is code, not a comment; a '--- sql' body line is counted.
func TestParseAndClassifyCodeLooksLikeComments(t *testing.T) {
	diff := "diff --git a/x.c b/x.c\n--- a/x.c\n+++ b/x.c\n@@ -1 +1,4 @@\n+#include <stdio.h>\n+*p = 1;\n--- old sql comment\n+// real\n"
	files := parseDiffFiles(diff)
	if len(files) != 1 || len(files[0].changes) != 4 {
		t.Fatalf("changes = %+v, want 4 body lines", files)
	}
	got := analyzeDiffFile(files[0])
	if len(got) != 1 || got[0].Line != "+// real" {
		t.Fatalf("findings = %+v, want only the // comment", got)
	}
	if classifyLine("+#!x", "run.sh") != "comment-only" || classifyLine("+# x", "Makefile") != "" {
		t.Error("# is a comment only for hash-comment extensions")
	}
}

// A bad range makes git exit non-zero; that must be an error, never an empty clean diff.
func TestGetDiffGitFailureIsError(t *testing.T) {
	var stderr bytes.Buffer
	if _, code := getDiff(t.TempDir(), "nosuch..range", "", &stderr); code != 1 || stderr.Len() == 0 {
		t.Fatalf("code %d stderr %q, want 1 and a message", code, stderr.String())
	}
}

// A range starting with '-' is a git option (--output overwrites a file); it must be rejected before git runs.
func TestRunGitDiffRejectsOptionRange(t *testing.T) {
	repo := hosttest.Repo(t)
	victim := filepath.Join(t.TempDir(), "victim")
	var stderr bytes.Buffer
	if _, ok := runGitDiff(repo, "--output="+victim, &stderr); ok || stderr.Len() == 0 {
		t.Fatalf("ok=%v stderr=%q, want rejection with a message", ok, stderr.String())
	}
	if _, err := os.Stat(victim); err == nil {
		t.Fatal("git ran with --output and created the file")
	}
}

func TestBuildDiffResultQuoteSwapCountsBothLines(t *testing.T) {
	got := buildDiffResult([]*diffFileEntry{{file: "app.py", changes: []string{`-value = "hello"`, `+value = 'hello'`}}})
	if got.TotalChangeLine != 2 || got.NoiseLines != 2 || got.NoiseRatio != 1 || got.Verdict != "VERY_NOISY" {
		t.Fatalf("quote swap = %+v; want 2 changes, 2 noise lines, ratio 1, VERY_NOISY", got)
	}
}
