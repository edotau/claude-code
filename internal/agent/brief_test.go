package agent

import (
	"os"
	"os/exec"
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

func TestBriefReadOnlyOwnsNothing(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Chdir(config)
	brief, err := RenderBrief(BriefInput{Task: "look", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(brief, "Owned: []\n") {
		t.Errorf("read-only brief should print an empty ownership list: %q", brief)
	}
	if got := ParseOwned(brief); len(got) != 0 {
		t.Errorf("read-only brief owns %q; want nothing", got)
	}
	if got := ParseOwned("Owned: null\nMode: read-only"); len(got) != 0 {
		t.Errorf("legacy null ownership parsed as %q", got)
	}
}

func assertBriefReportPath(t *testing.T, brief, config string) {
	t.Helper()
	key := filepath.Base(config)
	want := filepath.Join(config, "memory", "reports", key+"--YYYY-MM-DD-<name>.md")
	if got := strings.Count(brief, want); got != 2 {
		t.Errorf("brief has %d report paths matching %q; want 2", got, want)
	}
}

func assertBriefContract(t *testing.T, first string, input BriefInput) {
	t.Helper()
	for _, want := range []string{"## Leaf rules", "Never revert a change you did not make", input.Task, input.Report, input.Verify,
		"read-only; do not edit files", "Wrap lines at 118 columns", "Final answer: at most 8 lines",
		"returns with the job ended, hand back\n  the job's result as your next action; do nothing in between"} {
		if !strings.Contains(first, want) {
			t.Errorf("brief missing contract %q", want)
		}
	}
}

func TestBriefReportPathCarriesTheBankPrefix(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := filepath.Join(t.TempDir(), "alpha")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	t.Chdir(repo)
	in := BriefInput{Task: "x", ReadOnly: true, Verify: "true"}
	flat, err := RenderBrief(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flat, "/alpha--YYYY-MM-DD-<name>.md") {
		t.Errorf("flat brief lacks the key prefix:\n%s", flat)
	}
}
