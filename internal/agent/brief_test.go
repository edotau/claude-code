package agent

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestBriefParseOwned(t *testing.T) {
	input := BriefInput{
		Task:     "Implement {{literal task text}}.\nOwned: /tmp/not-owned.go",
		Owned:    []string{"/tmp/one.go", "/tmp/path with spaces.go", "/tmp/quote\".go"},
		ReadOnly: true,
		Report:   "/Users/edotau/.claude/memory/claude-code/reports/leaf.md",
		Verify:   "go test ./internal/agent",
	}
	first, err := RenderBrief(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderBrief(input)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !reflect.DeepEqual(ParseOwned(first), input.Owned) {
		t.Fatalf("brief is not deterministic or ownership did not round trip: %q", first)
	}
	assertBriefContract(t, first, input)
	if got := ParseOwned("Task only\nOwned: [broken"); got != nil {
		t.Errorf("malformed ownership accepted: %q", got)
	}
	if got := ParseOwned("Task only"); got != nil {
		t.Errorf("invented ownership: %q", got)
	}
	if got := ParseOwned("Owned: /tmp/a.go /tmp/b.go"); !reflect.DeepEqual(got, []string{"/tmp/a.go", "/tmp/b.go"}) {
		t.Errorf("legacy ownership: %q", got)
	}
}

func assertBriefContract(t *testing.T, first string, input BriefInput) {
	t.Helper()
	plan, err := os.ReadFile("../../plans/2026-11-13-permissions-settings-toolbox.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rules, ok := strings.Cut(string(plan), "## Leaf rules\n\n")
	if !ok {
		t.Fatal("plan has no leaf rules")
	}
	rules, _, _ = strings.Cut(rules, "\n## Decisions needed")
	for _, want := range []string{strings.TrimSpace(rules), input.Task, input.Report, input.Verify,
		"read-only; do not edit files", "Wrap lines at 118 columns", "Final answer: at most 8 lines"} {
		if !strings.Contains(first, want) {
			t.Errorf("brief missing contract %q", want)
		}
	}
}
