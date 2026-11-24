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

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/rtkwrap"
)

func TestBriefParseOwned(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Chdir(config)
	input := BriefInput{
		Task:   "Implement {{literal task text}}.\nOwned: /tmp/not-owned.go",
		Owned:  []string{"/tmp/one.go", "/tmp/path with spaces.go", "/tmp/quote\".go"},
		Report: filepath.Join(config, "memory", "reports", "leaf.md"),
		Verify: "go test ./internal/agent",
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
	want := "\nReport: " + filepath.Join(config, "memory", "reports", "leaf.md") + "\n"
	if got := strings.Count(brief, want); got != 1 {
		t.Errorf("brief has %d report paths matching %q; want 1", got, want)
	}
}

func assertBriefContract(t *testing.T, first string, input BriefInput) {
	t.Helper()
	for _, want := range []string{"## Leaf rules", "Never revert a change you did not make", input.Task, input.Report, input.Verify,
		"implementation; edit only owned files", "Wrap lines at 118 columns", "Final answer: at most 8 lines",
		"the full report path",
		"returns with the job ended, hand back\n  the job's result as your next action; do nothing in between"} {
		if !strings.Contains(first, want) {
			t.Errorf("brief missing contract %q", want)
		}
	}
	for _, once := range []string{"Final answer:"} {
		if got := strings.Count(first, once); got != 1 {
			t.Errorf("brief states %q %d times; want once", once, got)
		}
	}
	for _, want := range []string{"basic tests that a new feature works", "a bug fix shows its live\n  before/after run",
		"- A `python3` heredoc that rewrites a file is a splice script; write a helper script with Write, not heredoc + chmod.\n" +
			"- Every `rm` names a literal path: no `$(...)`, unquoted variable or glob; never `rm` in `bash -c`/`sh -c`.\n" +
			"- Never read/export live vault or provider config (`vault ls`, `vault get`) unless the brief names it; use fixtures.",
		"other cases are proposals in the hand-back", "Order: format → lint → live check → only a test you wrote or repair"} {
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

func TestBriefDropsRTKLineInCodeMode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	input := BriefInput{Task: "t", Owned: []string{"/tmp/a.go"}, Verify: "go test ./x"}
	for mode, want := range map[string]bool{"": true, "on": false} {
		t.Setenv(rtkwrap.CodemodeEnv, mode)
		brief, err := RenderBrief(input)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(brief, "goes through rtk"); got != want {
			t.Errorf("code mode %q: rtk line present = %v, want %v", mode, got, want)
		}
	}
}

func TestBriefCodeModeFollowsTarget(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, session := range []string{"", "on"} {
		t.Setenv(rtkwrap.CodemodeEnv, session)
		for _, target := range []bool{true, false} {
			brief, err := RenderBrief(BriefInput{Task: "t", Owned: []string{"/tmp/a.go"}, CodeMode: &target})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(brief, "goes through rtk"); got == target {
				t.Errorf("session %q target %v: rtk line present = %v", session, target, got)
			}
		}
	}
}

func TestBriefTypedTierAndPlanTask(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	brief, err := RenderBrief(BriefInput{Task: "fix it", Owned: []string{"a.go"}, PlanTask: "Next N7", Tier: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	if alias, _ := TaskTier(brief); alias != models.Haiku || !strings.Contains(brief, "\nTask: Next N7") {
		t.Fatalf("tier or task line missing: %q", brief)
	}
	if _, err := RenderBrief(BriefInput{Task: "t", Owned: []string{"a.go"}, Tier: "huge"}); err == nil {
		t.Fatal("unknown tier accepted")
	}
}

func TestBriefAlwaysBakesReportPath(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	in := BriefInput{Task: "t", Owned: []string{"a.go"}, PlanTask: "Next N16"}
	a, errA := RenderBrief(in)
	b, errB := RenderBrief(in)
	if errA != nil || errB != nil || a == b || !strings.Contains(a, "\nReport: ") || !strings.Contains(a, "-next-n16-") {
		t.Fatalf("report path not baked uniquely: %v %v\n%s", errA, errB, a)
	}
}
