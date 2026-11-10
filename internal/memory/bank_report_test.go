package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendReportRollsBackWhenDecisionsFail(t *testing.T) {
	tempConfig(t)
	previousNow := now
	now = func() time.Time { return time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = previousNow })
	for _, previous := range []string{"", "# Existing\n\nKeep these bytes.\t"} {
		t.Run(previous, func(t *testing.T) {
			dir := t.TempDir()
			report := filepath.Join(dir, "reports", "2026-11-08-review.md")
			decisions := filepath.Join(dir, "reports", "2026-11-08-decisions.md")
			if previous != "" {
				write(t, report, previous)
			}
			if err := os.MkdirAll(decisions, 0o700); err != nil {
				t.Fatal(err)
			}
			in := ReportInput{Report: "# Review\n\nUnique report body.", Decisions: []string{"Keep it."}}
			paths, err := AppendReport(dir, "review", in)
			if err == nil || len(paths) != 0 {
				t.Fatalf("failed decisions write = %v, %v; want no paths and an error", paths, err)
			}
			assertReportRolledBack(t, report, previous)
			if err := os.Remove(decisions); err != nil {
				t.Fatal(err)
			}
			if _, err := AppendReport(dir, "review", in); err != nil {
				t.Fatalf("retry report: %v", err)
			}
			assertReportTextOccursOnce(t, report, "Unique report body.")
			assertReportTextOccursOnce(t, decisions, "Keep it.")
		})
	}
}

func assertReportTextOccursOnce(t *testing.T, path, text string) {
	t.Helper()
	if got := strings.Count(mustRead(t, path), text); got != 1 {
		t.Fatalf("retry saved %q in %s %d times, want 1", text, path, got)
	}
}

func assertReportRolledBack(t *testing.T, path, previous string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if previous == "" {
		if !os.IsNotExist(err) {
			t.Fatalf("new report survived failed decisions write: %q, %v", got, err)
		}
		return
	}
	if err != nil || string(got) != previous {
		t.Fatalf("existing report not restored byte-for-byte: got %q, %v; want %q", got, err, previous)
	}
}

func TestDecisionLinesPreservesLiteralWhitespace(t *testing.T) {
	got := decisionLines("2026-11-08", "review", []string{
		"\t Keep `a  b\tc` and \"x  y\tz\"\r\nnext\nlast\rtail \t",
		" \t\r\n ",
	})
	want := "- 2026-11-08 · review · Keep `a  b\tc` and \"x  y\tz\" next last tail\n"
	if got != want {
		t.Fatalf("decision whitespace changed: got %q, want %q", got, want)
	}
}

func TestAppendTextSeparatesUnterminatedMarkdown(t *testing.T) {
	for _, tc := range []struct{ name, previous, text, want string }{
		{"heading", "# Existing", "# New\n", "# Existing\n# New\n"},
		{"bullet", "- Existing", "- New\n", "- Existing\n- New\n"},
		{"terminated", "# Existing\n", "# New\n", "# Existing\n# New\n"},
		{"empty", "", "# New\n", "# New\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.md")
			write(t, path, tc.previous)
			if err := appendText(path, "# Unused header\n\n", tc.text); err != nil {
				t.Fatal(err)
			}
			if got := mustRead(t, path); got != tc.want {
				t.Fatalf("append merged Markdown or added a blank line: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAppendReportRejectsDecisionsLabel(t *testing.T) {
	tempConfig(t)
	for _, label := range []string{"decisions", "DECISIONS", "Decisions"} {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			paths, err := AppendReport(dir, label, ReportInput{
				Report:    "# Review\n\nReport body.\n",
				Decisions: []string{"Keep reports separate from decisions."},
			})
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("AppendReport(%q) = %v, %v; want reserved-label error", label, paths, err)
			}
			if len(paths) != 0 {
				t.Fatalf("rejected report returned paths: %v", paths)
			}
			if _, err := os.Stat(filepath.Join(dir, "reports")); !os.IsNotExist(err) {
				t.Fatalf("rejected label created reports directory: %v", err)
			}
		})
	}
}

// A failed store commit rolls back both appends, so a retry never duplicates the report or its decisions.

// not a repo: commitStore fails
