package diff

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
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
	missing := filepath.Join("testdata", "report", "nosuchdir")
	abs, err := filepath.Abs(missing)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := RunReport([]string{missing}, &stdout, &stderr)
	if want := "Error: Path does not exist: " + abs + "\n"; code != 1 || stdout.Len() != 0 || stderr.String() != want {
		t.Fatalf("RunReport = %d, stdout %q, stderr %q; want 1, empty, %q", code, &stdout, &stderr, want)
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

// An unknown flag exits 2 with the flag error and the verb's own usage page on stderr, nothing on stdout.
func TestReviewCommandsRejectUnknownFlag(t *testing.T) {
	for _, tc := range []struct {
		verb string
		run  func([]string, io.Writer, io.Writer) int
	}{{"pr", RunPR}, {"report", RunReport}, {"diff", RunDiff}} {
		t.Run(tc.verb, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := tc.run([]string{"--nope"}, &stdout, &stderr)
			want := "flag provided but not defined: -nope\nusage: claude-code review " + tc.verb + " "
			if code != 2 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), want) {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 2, empty, prefix %q", code, &stdout, &stderr, want)
			}
		})
	}
}

// testdataDir is testdata as RunReport prints it (Abs of cwd, symlinks kept), so goldens stay machine-independent.
var testdataDir, _ = filepath.Abs("testdata")

// expandGolden swaps the @TESTDATA@ placeholder in a golden for this checkout's testdata path.
func expandGolden(s string) string { return strings.ReplaceAll(s, "@TESTDATA@", testdataDir) }

// One action item per category must list every affected file and carry the worst severity, in any finding order.
func TestGenerateActionItemsMergesCategory(t *testing.T) {
	fs := []Finding{
		{Category: "x", Severity: "medium", File: "a.go"},
		{Category: "x", Severity: "critical", File: "b.go"},
		{Category: "x", Severity: "high", File: "c.go"},
	}
	items := generateActionItems(fs)
	if len(items) != 1 {
		t.Fatalf("items %v, want 1", items)
	}
	it := items[0]
	if it.Priority != "P0" || it.Severity != "critical" || strings.Join(it.FilesAffected, ",") != "a.go,b.go,c.go" {
		t.Fatalf("got %+v, want P0 critical with all three files", it)
	}
}

func TestGenerateReportFailsWhenQualityDiffFails(t *testing.T) {
	_, err := generateReport(t.TempDir(), "nobase", "nohead", &prInput{Status: "skipped"}, nil)
	if err == nil || !strings.Contains(err.Error(), "git diff") {
		t.Fatalf("generateReport() error = %v, want a git diff failure, not a clean quality pass", err)
	}
}

func TestGenerateReportSingleFileQualitySummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.json")
	body := `{"file": "app.py", "quality_score": 72.5, "smells": [{"type": "long_method"}, {"type": "magic_number"}],` +
		` "solid_violations": [{"principle": "SRP"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	quality, err := loadQualityAnalysisFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := generateReport("", "", "", &prInput{Status: "no_changes"}, quality)
	want := &QualitySummary{FilesAnalyzed: 1, AverageScore: 72.5, TotalCodeSmells: 2, TotalSolidViolations: 1}
	if err != nil || !reflect.DeepEqual(got.QualitySummary, want) {
		t.Fatalf("single-file summary = %+v, %v; want %+v", got.QualitySummary, err, want)
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

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
