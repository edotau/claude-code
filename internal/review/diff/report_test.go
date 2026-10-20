package diff

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// golden commands (captured from testdata/report, python3 on PATH):
//   python3 review_report_generator.py repo --pr-analysis pr-fixture.json --quality-analysis quality-fixture.json [--format markdown|--json]
// pr-fixture.json / quality-fixture.json are hand-written stand-ins for pr_analyzer.py /
// code_quality_checker.py output — this keeps RunReport's own test hermetic (no live git).

var (
	textTimestampRe     = regexp.MustCompile(`(?m)^Generated: .*$`)
	markdownTimestampRe = regexp.MustCompile(`(?m)^\*\*Generated:\*\* .*$`)
	generatedAtRe       = regexp.MustCompile(`"generated_at": "[^"]*"`)
)

func normalizeReportTimestamp(s string) string {
	s = textTimestampRe.ReplaceAllString(s, "Generated: <TIMESTAMP>")
	s = markdownTimestampRe.ReplaceAllString(s, "**Generated:** <TIMESTAMP>")
	return generatedAtRe.ReplaceAllString(s, `"generated_at": "<TIMESTAMP>"`)
}

func readReportGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "report", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return expandGolden(string(b))
}

func TestTitleWords(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"critical high medium low", "Critical High Medium Low"},
		{"\tcritical  HIGH\nmedium\rLOW\vlow\fcat", "\tCritical  HIGH\nMedium\rLOW\vLow\fCat"},
		{"cRITICAL lowerCase", "CRITICAL LowerCase"},
	} {
		if got := titleWords(tc.in); got != tc.want {
			t.Errorf("titleWords(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRunReportMatchesGolden(t *testing.T) {
	baseArgs := []string{
		filepath.Join("testdata", "report", "repo"),
		"--pr-analysis", filepath.Join("testdata", "report", "pr-fixture.json"),
		"--quality-analysis", filepath.Join("testdata", "report", "quality-fixture.json"),
	}
	cases := []struct {
		name  string
		extra []string
		want  string
	}{
		{"text", nil, "text.want"},
		{"markdown", []string{"--format", "markdown"}, "markdown.want"},
		{"json", []string{"--json"}, "json.want.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append(append([]string{}, baseArgs...), tc.extra...)
			code := RunReport(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			want := normalizeReportTimestamp(readReportGolden(t, tc.want))
			got := normalizeReportTimestamp(stdout.String())
			if got != want {
				t.Errorf("output mismatch (timestamp normalized):\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

func TestRunReportMissingRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunReport([]string{filepath.Join("testdata", "report", "nosuchdir")}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestRunReportRejectsInvalidPRSummary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		file  string
		field string
	}{
		{name: "string integer", file: "pr-summary-string.json", field: "files_changed"},
		{name: "fractional integer", file: "pr-summary-fraction.json", field: "total_additions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunReport([]string{
				filepath.Join("testdata", "report", "repo"),
				"--pr-analysis", filepath.Join("testdata", "report", tc.file),
				"--quality-analysis", filepath.Join("testdata", "report", "quality-fixture.json"),
				"--json",
			}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty output on invalid PR summary", stdout.String())
			}
			if !strings.Contains(stderr.String(), "summary") || !strings.Contains(stderr.String(), tc.field) {
				t.Errorf("stderr = %q, want useful context for summary.%s", stderr.String(), tc.field)
			}
		})
	}
}

func TestGenerateReportReturnsPRSummaryMarshalError(t *testing.T) {
	_, err := generateReport("", "", "", map[string]any{
		"status":  "analyzed",
		"summary": map[string]any{"unsupported": make(chan int)},
	}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "summary") {
		t.Fatalf("generateReport() error = %v, want a contextual summary marshal error", err)
	}
}

func TestMarkdownReportSectionLimits(t *testing.T) {
	var r Report
	for i := range 12 {
		r.ActionItems = append(r.ActionItems, ActionItem{
			Priority: "P0", Action: fmt.Sprintf("action-%02d", i),
			FilesAffected: []string{"a", "b", "c", "hidden-file"},
		})
		r.Findings = append(r.Findings,
			Finding{Severity: "critical", Category: "critical", Message: fmt.Sprintf("critical-%02d", i)},
			Finding{Severity: "high", Category: "high", Message: fmt.Sprintf("high-%02d", i)},
		)
		r.ReviewOrder = append(r.ReviewOrder, fmt.Sprintf("review-%02d", i))
	}
	got := formatMarkdownReport(r)
	for _, want := range []string{"action-11", "critical-11", "high-09", "review-09", "Files: a, b, c\n", "in `unknown`"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"hidden-file", "high-10", "high-11", "review-10", "review-11"} {
		if strings.Contains(got, absent) {
			t.Errorf("unexpected %q in report", absent)
		}
	}
}

func TestRunReportBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunReport([]string{"--nope"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit code = 0 for a bad flag, want nonzero")
	}
}
