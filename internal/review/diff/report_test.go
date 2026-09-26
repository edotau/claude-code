package diff

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
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

func TestRunReportBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunReport([]string{"--nope"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit code = 0 for a bad flag, want nonzero")
	}
}
