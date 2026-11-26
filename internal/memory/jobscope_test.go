package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

func TestJobToolsUnknownHoldsNone(t *testing.T) {
	for _, kind := range []string{"digest", "nonsense", ""} {
		if memory.JobTools(kind) {
			t.Errorf("JobTools(%q) = true, want false", kind)
		}
	}
	if !memory.JobTools(memory.JobUpdate) || !memory.JobTools(memory.JobDream) {
		t.Error("update or dream reports no tools")
	}
}

// planCall is one plan command a fake dream issues and what the verb does.
type planCall struct {
	cmd string
	do  func() error
}

// planFixture is a repo whose main has a commit touching a.go, a side-branch commit, a note and a plan of four tasks.
func planFixture(t *testing.T) (work, plan, good, side string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	work = hosttest.RepoAt(t, filepath.Join(t.TempDir(), "proj"))
	hosttest.WriteRel(t, work, "a.go", "package a\n")
	hosttest.In(t, work, "add", "a.go")
	hosttest.In(t, work, "commit", "-q", "-m", "feat: a")
	good = strings.TrimSpace(hosttest.In(t, work, "rev-parse", "--short=7", "HEAD"))
	hosttest.In(t, work, "switch", "-q", "-c", "side")
	hosttest.WriteRel(t, work, "b.go", "package a\n")
	hosttest.In(t, work, "add", "b.go")
	hosttest.In(t, work, "commit", "-q", "-m", "feat: b")
	side = strings.TrimSpace(hosttest.In(t, work, "rev-parse", "--short=7", "HEAD"))
	hosttest.In(t, work, "switch", "-q", "main")
	hosttest.WriteRel(t, paths.MemoryStoreDir(), "feedback/x.md", "x\n")
	plan = filepath.Join(plans.Home(work), time.Now().Format(time.DateOnly)+"-e2e.md")
	hosttest.WriteRel(t, filepath.Dir(plan), filepath.Base(plan), "# P\n\n- [ ] Phase 1.1 One — owns: a.go\n- [ ] Phase 1.2 Two\n"+
		"- [ ] Phase 1.3 Three — owns: b.go\n- [x] Phase 1.4 Four ("+good+")\n")
	return work, plan, good, side
}

// A fake dream's plan calls all run; Go keeps the proven ones, reopens the bad sha and logs each.
func TestDreamPlanPhaseKeepsProvenChangesOnly(t *testing.T) {
	work, plan, good, side := planFixture(t)
	task := "claude-code plan task "
	calls := []planCall{
		{task + "'Phase 1.1' --done --sha " + good + " --plan " + plan, func() error { _, err := plans.MarkDone(plan, "Phase 1.1", good); return err }},
		{task + "'Phase 1.2' --strike 'moot — evidence: note:feedback/x.md' --plan " + plan,
			func() error { return plans.Strike(plan, "Phase 1.2", "moot — evidence: note:feedback/x.md") }},
		{task + "'Phase 1.3' --done --sha " + side + " --plan " + plan, func() error { _, err := plans.MarkDone(plan, "Phase 1.3", side); return err }},
	}
	prompt := ""
	// The plan phase belongs to the legacy dream, so the store is stamped flat 3.
	hosttest.WriteRel(t, paths.MemoryStoreDir(), ".layout", "flat 3\n")
	run := func(_ context.Context, p, _ string) (string, error) {
		prompt = p
		for _, c := range calls {
			if err := c.do(); err != nil {
				t.Error(err)
			}
		}
		return "fake", nil
	}
	var out strings.Builder
	if err := memory.RunDream(memory.DreamRunOptions{CWD: work, Force: true, Out: &out, Run: run}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(plan)
	if !strings.Contains(prompt, `"id":"Phase 1.3"`) || !strings.Contains(out.String(), "plans: kept 2, reverted 1") {
		t.Fatalf("prompt lists open tasks %v, out:\n%s", strings.Contains(prompt, "Open plan tasks"), out.String())
	}
	for _, want := range []string{"[x] Phase 1.1 One — owns: a.go (" + good + ")", "~~Phase 1.2 Two~~ — superseded: moot",
		"[ ] Phase 1.3 Three — owns: b.go\n", "[x] Phase 1.4 Four"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("plan lacks %q:\n%s", want, body)
		}
	}
	if log, _ := os.ReadFile(filepath.Join(paths.StateDir(), "memory", "plans-audit.jsonl")); strings.Count(string(log), `"verdict":"kept"`) != 2 ||
		!strings.Contains(string(log), `"verdict":"reverted: sha is not on the trunk branch"`) {
		t.Errorf("log:\n%s", log)
	}
}
