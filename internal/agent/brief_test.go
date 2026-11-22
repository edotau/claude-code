package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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
	want := filepath.Join(config, "memory", "reports", "YYYY-MM-DD-<name>.md")
	if got := strings.Count(brief, want); got != 0 { // a read-only brief's report is its answer, filed by --save
		t.Errorf("brief has %d report paths matching %q; want 0", got, want)
	}
}

func assertBriefContract(t *testing.T, first string, input BriefInput) {
	t.Helper()
	for _, want := range []string{"## Leaf rules", "Never revert a change you did not make", input.Task, input.Report, input.Verify,
		"read-only; do not edit files", "Wrap lines at 118 columns", "Final answer: the full report", "--save files it",
		"returns with the job ended, hand back\n  the job's result as your next action; do nothing in between"} {
		if !strings.Contains(first, want) {
			t.Errorf("brief missing contract %q", want)
		}
	}
	for _, once := range []string{"Final answer:", "--save files it", "gross production lines"} {
		if got := strings.Count(first, once); got != 1 {
			t.Errorf("brief states %q %d times; want once", once, got)
		}
	}
	for _, want := range []string{"basic tests that a new feature works", "a bug fix shows its live\n  before/after run",
		"- A `python3` heredoc that rewrites a file is a splice script.\n" +
			"- Every `rm` names a literal path: no `$(...)`, unquoted variable or glob; never `rm` in `bash -c`/`sh -c`.\n" +
			"- Never read/export live vault or provider config (`vault ls`, `vault get`) unless the brief names it; use fixtures.",
		"other cases are proposals in the hand-back", "Order: format → lint → live check → your package or exact tests"} {
		if !strings.Contains(first, want) {
			t.Errorf("brief missing restored rule %q", want)
		}
	}
}

func TestBriefPathAllocation(t *testing.T) {
	dir := t.TempDir()
	oldDir, oldRandom := briefDir, briefRandom
	briefDir = func() string { return dir }
	t.Cleanup(func() { briefDir, briefRandom = oldDir, oldRandom })
	stale, other := ageBriefFixtures(t, dir)
	var wg sync.WaitGroup
	const allocations = 200
	paths := make(chan string, allocations)
	for range allocations {
		wg.Go(func() {
			path, err := NewBriefPath("codex")
			if err != nil {
				t.Error(err)
				return
			}
			paths <- path
		})
	}
	wg.Wait()
	close(paths)
	assertBriefPaths(t, paths, allocations)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale brief retained: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal(err)
	}
	assertBriefExclusive(t)
}

func assertBriefExclusive(t *testing.T) {
	t.Helper()
	briefRandom = bytes.NewReader(make([]byte, 16))
	first, err := NewBriefPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	briefRandom = bytes.NewReader(append(make([]byte, 16), bytes.Repeat([]byte{1}, 16)...))
	second, err := NewBriefPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || string(content) != "keep" {
		t.Fatal("returned or modified an existing brief")
	}
}

func ageBriefFixtures(t *testing.T, dir string) (string, string) {
	t.Helper()
	stale := filepath.Join(dir, "claude-code-brief-20000101-codex-"+strings.Repeat("a", 32)+".md")
	other := filepath.Join(dir, "unrelated.md")
	for _, path := range []string{stale, other} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-8 * 24 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return stale, other
}

func assertBriefPaths(t *testing.T, paths <-chan string, allocations int) {
	t.Helper()
	seen := make(map[string]bool)
	for path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if seen[path] || !filepath.IsAbs(path) || info.Size() != 0 {
			t.Fatalf("invalid allocation %q", path)
		}
		seen[path] = true
	}
	if len(seen) != allocations {
		t.Fatalf("got %d paths", len(seen))
	}
}
