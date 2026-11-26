package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/waves"
	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
)

// planRepo is a repo (the cwd) whose plans home holds one plan per name=body pair.
func planRepo(t *testing.T, named map[string]string) string {
	t.Helper()
	cfgDir(t)
	repo := paths.Real(hosttest.Repo(t))
	t.Chdir(repo)
	for name, body := range named {
		hosttest.WriteRel(t, plans.Home(repo), name, body)
	}
	return repo
}

func readPlan(t *testing.T, repo, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(plans.Home(repo), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// plan status prints the dropped count only for a plan that has dropped tasks.
func TestPlanStatusRowsShowDroppedOnlyWhenPresent(t *testing.T) {
	planRepo(t, map[string]string{
		"2026-11-01-a.md": "- [x] 1.1 one\n- [ ] 1.2 two\n- [ ] ~~1.3 three~~ — superseded: x\n",
		"2026-11-02-b.md": "- [x] 2.1 one\n- [ ] 2.2 two\n",
	})
	code, out, _ := runCLI(t, "plan", "status")
	if code != 0 || !strings.Contains(out, "1/2 (1 dropped)  ") || !strings.Contains(out, "1/2  ") || strings.Count(out, "dropped") != 1 {
		t.Fatalf("exit %d, out:\n%s", code, out)
	}
}

// plan task: done, reopen and strike by hand, and a refusal naming every plan when the id is not unique.
func TestPlanTaskDoneReopenStrikeAndAmbiguousId(t *testing.T) {
	repo := planRepo(t, map[string]string{
		"2026-11-01-a.md": "- [ ] 1.1 Manual step\n- [ ] 1.2 Two lines first part\n  and the second part\n",
		"2026-11-02-b.md": "- [ ] 1.1 Same id elsewhere\n",
	})
	code, _, stderr := runCLI(t, "plan", "task", "1.1", "--done")
	if code == 0 || !strings.Contains(stderr, "2026-11-01-a.md") || !strings.Contains(stderr, "2026-11-02-b.md") {
		t.Fatalf("a non-unique id must be refused naming both plans: exit %d %q", code, stderr)
	}
	a := filepath.Join(plans.Home(repo), "2026-11-01-a.md")
	steps := []struct {
		args []string
		want string
	}{
		{[]string{"plan", "task", "1.1", "--done", "--sha", "abc1234", "--plan", a}, "- [x] 1.1 Manual step (abc1234)\n"},
		{[]string{"plan", "task", "1.1", "--reopen", "--plan", a}, "- [ ] 1.1 Manual step <!-- reopened "},
		{[]string{"plan", "task", "1.2", "--strike", "folded in", "--plan", a},
			"- [ ] ~~1.2 Two lines first part and the second part~~ — superseded: folded in\n"},
	}
	for _, s := range steps {
		if code, _, stderr := runCLI(t, s.args...); code != 0 || !strings.Contains(readPlan(t, repo, "2026-11-01-a.md"), s.want) {
			t.Fatalf("%v: exit %d %q\n%s", s.args, code, stderr, readPlan(t, repo, "2026-11-01-a.md"))
		}
	}
	if code, _, _ := runCLI(t, "plan", "task", "1.1"); code == 0 {
		t.Fatal("no mode must fail")
	}
}

// plan task marks a task in a lane and ticks one by its #index; plan status shows the mark; an index needs --plan.
func TestPlanTaskLaneMarkAndIndex(t *testing.T) {
	const name = "2026-11-01-a.md"
	repo := planRepo(t, map[string]string{name: "## Tasks\n\n- [ ] 1.1 First\n- [ ] 1.2 Second\n- [ ] Phase 7.2 x — owns: c.go\n"})
	file := filepath.Join(plans.Home(repo), name)
	for _, c := range []struct {
		args []string
		code int
		want string // in stdout or stderr
	}{
		{[]string{"plan", "task", "#1", "--lane", "lane-a", "--sha", "abc1234", "--plan", file}, 0, ""},
		{[]string{"plan", "task", "#2", "--done", "--sha", "def5678", "--plan", file}, 0, ""},
		{[]string{"plan", "status"}, 0, "1.1: in lane lane-a (abc1234)"},
		{[]string{"plan", "task", "#1", "--done"}, 1, "pass --plan"},
		{[]string{"plan", "task", "1.1", "--lane", "lane-a"}, 2, ""}, // --lane without --sha is a usage error
		{[]string{"plan", "task", "Phase 7.9", "--add", "--plan", file, "--title", "New", "--leaf", "claude", "--agent", "pi", "--owns", "d.go"}, 0,
			"Phase 7.2: missing leaf:"}, // an old malformed line is a note on --add
	} {
		code, out, stderr := runCLI(t, c.args...)
		if code != c.code || !strings.Contains(out+stderr, c.want) {
			t.Fatalf("%q: exit %d, want %d with %q:\n%s%s", c.args, code, c.code, c.want, out, stderr)
		}
	}
	got := readPlan(t, repo, name)
	for _, want := range []string{"- [ ] 1.1 First <!-- lane lane-a: abc1234 -->\n- [x] 1.2 Second (def5678)\n", "Phase 7.9 — New"} {
		if !strings.Contains(got, want) {
			t.Fatalf("plan lacks %q:\n%s", want, got)
		}
	}
}

func TestPlanMutationsRejectExtraArguments(t *testing.T) {
	const name, body = "2026-11-18-safe.md", "- [ ] 1.1 Safe\n"
	repo := planRepo(t, map[string]string{name: body})
	file := filepath.Join(plans.Home(repo), name)
	for _, args := range [][]string{
		{"plan", "task", "1.1", "--done", "extra", "--plan", file},
		{"plan", "task", "--done", "1.1", "extra", "--plan", file},
		{"plan", "task", "", "--done", "--plan", file},
		{"plan", "tick", "--audit", "extra"},
		{"plan", "tick", "--verify", "--dry-run"},
		{"plan", "tick", "--verify", "--range", "HEAD..HEAD"},
		{"plan", "tick"},
	} {
		code, out, stderr := runCLI(t, args...)
		if code != 2 || out != "" || readPlan(t, repo, name) != body {
			t.Fatalf("%q: exit %d, out %q, stderr %q; plan %q", args, code, out, stderr, readPlan(t, repo, name))
		}
	}
	code, _, stderr := runCLI(t, "plan", "task", "--done", "1.1", "--plan", file, "--sha", "abc1234")
	if code != 0 || !strings.Contains(readPlan(t, repo, name), "- [x] 1.1 Safe (abc1234)") {
		t.Fatalf("interspersed flags: exit %d, stderr %q", code, stderr)
	}
}

// plan task is offered by completion, so TestCompletionMatchesHelpFlags then holds its flags to -h.
func TestPlanSpecOffersTask(t *testing.T) {
	if planSpec().Sub["task"] == nil {
		t.Fatal("completion spec has no plan task")
	}
}

func TestPlanExplicitBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := planRepo(t, map[string]string{"2026-11-18-safe.md": "- [ ] 1.1 Safe\n", "draft.md": "- [ ] 1.1 Draft\n"})
	home := plans.Home(repo)
	outside := filepath.Join(t.TempDir(), "2026-11-18-outside.md")
	hosttest.WriteRel(t, filepath.Dir(outside), filepath.Base(outside), "- [ ] 1.1 Outside\n")
	link := filepath.Join(home, "2026-11-18-link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	insideLink := filepath.Join(home, "2026-11-18-inside-link.md")
	if err := os.Symlink(filepath.Join(home, "2026-11-18-safe.md"), insideLink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, link, insideLink, filepath.Join(home, "draft.md"), home, filepath.Join(home, "..", filepath.Base(outside))} {
		if _, _, err := plans.TaskTrailers(repo, path, []string{"1.1"}); err == nil {
			t.Errorf("commit accepted %s", path)
		}
		code, _, stderr := runCLI(t, "plan", "tick", "--range", "HEAD..HEAD", "--plan", path)
		if code != 2 || !strings.Contains(stderr, home) {
			t.Errorf("tick %s: exit=%d stderr=%q", path, code, stderr)
		}
		code, _, stderr = runCLI(t, "plan", "task", "1.1", "--done", "--plan", path)
		if code != 2 || !strings.Contains(stderr, home) || strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
			t.Errorf("%s: exit=%d stderr=%q", path, code, stderr)
		}
	}
	for _, name := range []string{"2026-11-18-safe.md", "2026-11-18-safe"} {
		code, _, stderr := runCLI(t, "plan", "task", "1.1", "--done", "--plan", name)
		if code != 0 {
			t.Errorf("%s: exit=%d %s", name, code, stderr)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "- [ ] 1.1 Outside\n" {
		t.Fatalf("outside changed: %s %v", got, err)
	}
}

// redGateOutput is empty for green or skipped checks and carries only a failed check's saved output.
func TestRedGateOutput(t *testing.T) {
	red := filepath.Join(t.TempDir(), "red.txt")
	if err := os.WriteFile(red, []byte("FAIL gofmt (0.0s)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	green := map[string][]waves.TaskCheck{"1.1": {{Exit: 0, Saved: red}, {Exit: 1, Skip: "skipped", Saved: red}}}
	if got := redGateOutput(green); got != "" {
		t.Fatalf("green checks = %q, want empty", got)
	}
	green["1.2"] = []waves.TaskCheck{{Exit: 1, Saved: red}}
	if got := redGateOutput(green); got != "FAIL gofmt (0.0s)\n" {
		t.Fatalf("red checks = %q", got)
	}
}

// a green tick still sweeps fix-leaf spend; autofix gets no gate output.
func TestTickFollowUpSweepsGreen(t *testing.T) {
	oldSweep, oldFix := tickSweep, tickAutofix
	t.Cleanup(func() { tickSweep, tickAutofix = oldSweep, oldFix })
	var swept int
	tickSweep = func(time.Time) []string { swept++; return nil }
	tickAutofix = func(_, _, out string, _ ...string) []string {
		if out != "" {
			t.Errorf("autofix gate output = %q, want empty", out)
		}
		return nil
	}
	tickFollowUp("", t.TempDir(), "abcdef0", map[string][]waves.TaskCheck{"1.1": {{Exit: 0}}}, nil)
	if swept != 1 {
		t.Fatalf("sweep calls = %d, want 1", swept)
	}
}

// plan tick --audit refuses --range and --plan with usage, and prints one JSON object on --dry-run.
func TestPlanTickAuditFlags(t *testing.T) {
	planRepo(t, map[string]string{"2026-11-18-a.md": "- [ ] 1.1 One\n"})
	for _, extra := range [][]string{{"--range", "a..b"}, {"--plan", "2026-11-18-a.md"}} {
		if code, _, stderr := runCLI(t, append([]string{"plan", "tick", "--audit"}, extra...)...); code != 2 {
			t.Errorf("%v: exit=%d %s", extra, code, stderr)
		}
	}
	code, stdout, stderr := runCLI(t, "plan", "tick", "--audit", "--dry-run", "--format", "json")
	if code != 0 || !strings.Contains(stdout, `"dry_run": true`) || !strings.Contains(stdout, `"changed": []`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

// a failing check is broadcast once per sha, and re-reporting after done reopens it.
func TestReportVerifyFailures(t *testing.T) {
	planRepo(t, nil)
	tasks := []plans.Task{{ID: "1.1"}, {ID: "1.2"}}
	checks := map[string][]waves.TaskCheck{"1.1": {{Exit: 1}}, "1.2": {{Exit: 1, Skip: "no verify"}}}
	ids := reportVerifyFailures("/p/2026-11-18-a.md", "/repo", "abcdef0123", tasks, checks)
	if len(ids) != 1 {
		t.Fatalf("ids = %q, want only the confirmed failure", ids)
	}
	reportVerifyFailures("/p/2026-11-18-a.md", "/repo", "abcdef0123", tasks, checks)
	if recs, _ := worktree.ListFailures(worktree.FailuresDir()); len(recs) != 1 {
		t.Fatalf("records = %d, want 1 (deduped)", len(recs))
	}
	_ = worktree.DoneFailure(worktree.FailuresDir(), ids[0])
	reportVerifyFailures("/p/2026-11-18-a.md", "/repo", "abcdef0123", tasks, checks)
	if _, done := worktree.FailureHolder(worktree.FailuresDir(), ids[0]); done {
		t.Fatal("a done failure stayed done after re-report")
	}
}

// a written verified tick appends the ready line to its notice; no tick, no line.
func TestTickGreenPostsReadyLine(t *testing.T) {
	name := time.Now().Format(time.DateOnly) + "-a"
	repo := planRepo(t, map[string]string{name + ".md": "- [ ] 1.1 One owns: a.go\n"})
	hosttest.WriteRel(t, repo, "a.go", "package a\n")
	hosttest.In(t, repo, "add", "a.go")
	hosttest.In(t, repo, "commit", "-q", "-m", "feat: a\n\nTask: "+name+" 1.1")
	sha := strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "HEAD"))
	old := tickReady
	t.Cleanup(func() { tickReady = old })
	var calls int
	tickReady = func(string, string) []string { calls++; return []string{"plans: READYLINE"} }
	p, err := plans.Parse(filepath.Join(plans.Home(repo), name+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if code := tickGreen("", repo, sha, p.Tasks, map[string]bool{"1.2": true}); code != 0 || calls != 0 {
		t.Fatalf("no green task: code %d, ready calls %d, want 0 and 0", code, calls)
	}
	_, out, _ := hosttest.CaptureOutput(t, func() int { return tickGreen("", repo, sha, p.Tasks, map[string]bool{"1.1": true}) })
	if calls != 1 || !strings.Contains(out, "READYLINE") {
		t.Fatalf("ready calls %d, output %q", calls, out)
	}
}
