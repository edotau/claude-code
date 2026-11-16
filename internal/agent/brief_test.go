package agent

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBriefParseOwned(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Chdir(config)
	input := BriefInput{
		Task:     "Implement {{literal task text}}.\nOwned: /tmp/not-owned.go",
		Owned:    []string{"/tmp/one.go", "/tmp/path with spaces.go", "/tmp/quote\".go"},
		ReadOnly: true,
		Report:   filepath.Join(config, "memory", "reports", "leaf.md"),
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
	assertBriefReportPath(t, first, config)
	if strings.Contains(first, "/Users/") {
		t.Fatalf("brief contains a hardcoded user path: %q", first)
	}
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

func assertBriefReportPath(t *testing.T, brief, config string) {
	t.Helper()
	key := filepath.Base(config)
	want := filepath.Join(config, "memory", key, "reports") + "/" + key + "--YYYY-MM-DD-<name>.md"
	if got := strings.Count(brief, want); got != 2 {
		t.Errorf("brief has %d report paths matching %q; want 2", got, want)
	}
}

func assertBriefContract(t *testing.T, first string, input BriefInput) {
	t.Helper()
	for _, want := range []string{"## Leaf rules", "Never revert a change you did not make", input.Task, input.Report, input.Verify,
		"read-only; do not edit files", "Wrap lines at 118 columns", "Final answer: at most 8 lines"} {
		if !strings.Contains(first, want) {
			t.Errorf("brief missing contract %q", want)
		}
	}
}
