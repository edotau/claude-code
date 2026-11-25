package contract

import (
	"strings"
	"testing"
)

func TestReportRequiresEveryField(t *testing.T) {
	sections := map[string]string{}
	for _, field := range ReportFields() {
		sections[field] = "Proof, or why not applicable."
	}
	sections["Gross production lines added and deleted"] = "Added: 0\nDeleted: 0\nScratch: 1 added, not production."

	if ps := ReportProblems(renderReport(sections, "", false)); len(ps) != 0 {
		t.Fatal(ps)
	}
	for _, field := range ReportFields() {
		for _, empty := range []bool{false, true} {
			if ps := ReportProblems(renderReport(sections, field, empty)); len(ps) == 0 {
				t.Fatalf("missing/empty %q passed", field)
			}
		}
		if !strings.Contains(ReportInstructions(), "## "+field) {
			t.Fatalf("brief omits %q", field)
		}
	}
}

func TestReportRejectsBadCounts(t *testing.T) {
	for _, bad := range []string{"Added: -1", "Added: scratch", "Added: 1/0"} {
		body := "## Gross production lines added and deleted\n" + bad + "\nDeleted: 0\n"
		if productionCount(body, "Added") {
			t.Fatalf("bad production count %q passed", bad)
		}
	}
}

func renderReport(sections map[string]string, omit string, empty bool) string {
	var body strings.Builder
	for _, field := range ReportFields() {
		if field == omit && !empty {
			continue
		}
		body.WriteString("## " + field + "\n")
		if field != omit {
			body.WriteString(sections[field] + "\n")
		}
	}
	return body.String()
}

func TestReportSectionsHandleSubheadsAndFencedExamples(t *testing.T) {
	body := "# Example\n```markdown\n## Outcome\nFake\n```\n## Commands and exit codes\n### Gate\n```sh\ncommand\n## Not a heading\n```\n## Observable proof\n### Receipt\nActual proof.\n"
	got := reportSections(body)
	if got["Outcome"] != "" {
		t.Fatal("fenced sample counted as a report field")
	}
	if !strings.Contains(got["Commands and exit codes"], "command") || !strings.Contains(got["Observable proof"], "Actual proof.") {
		t.Fatalf("subsections/code lost: %v", got)
	}
}

func TestReportInlineBackticksAndBulletCounts(t *testing.T) {
	sections := reportSections("```x```\n## Outcome\nDONE\n")
	if strings.TrimSpace(sections["Outcome"]) != "DONE" {
		t.Fatalf("inline code swallowed field: %v", sections)
	}
	for _, prefix := range []string{"", "- ", "* "} {
		if !productionCount(prefix+"Added: 3\n", "Added") {
			t.Fatal("bullet count refused", prefix)
		}
	}
}

func TestReportFenceClosingRequiresNoInfoString(t *testing.T) {
	body := "~~~language~~~\n## Outcome\nFake\n~~~\n" +
		"```go\n```go\n## Outcome\nStill fake\n```\n## Outcome\nDONE\n"
	sections := reportSections(body)
	if strings.TrimSpace(sections["Outcome"]) != "DONE" {
		t.Fatalf("code became report headings: %v", sections)
	}
}

func TestReportAcceptsFormattedCountsAndClosingHashes(t *testing.T) {
	for _, body := range []string{"**Added:** 3", "- **Added**: 3", "* __Added:__ 3", "Added: `3`"} {
		if !productionCount(body, "Added") {
			t.Fatal("formatted count refused", body)
		}
	}
	got := reportSections("    ```\n## Outcome ##\nDONE\n")
	if strings.TrimSpace(got["Outcome"]) != "DONE" {
		t.Fatalf("heading/fence indentation: %v", got)
	}
}
