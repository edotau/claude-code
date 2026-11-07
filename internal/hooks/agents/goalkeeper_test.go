package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/goal"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// gkEnv gives the test its own config dir and returns the session dir of session "sess1".
func gkEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	dir := filepath.Join(root, "projects", "p", "sess1")
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func gkStop(session string) core.Result {
	return GoalKeeper(core.ParseInput(strings.NewReader(`{"session_id":"` + session + `","hook_event_name":"Stop"}`)))
}

func gkLaunch(dir, run, toolInput string) {
	payload := fmt.Sprintf(`{"session_id":"sess1","transcript_path":%q,"cwd":"/tmp","hook_event_name":"PostToolUse",`+
		`"tool_name":"Workflow","tool_input":%s,"tool_response":"Run ID: %s"}`, dir+".jsonl", toolInput, run)
	registerWorkflow(core.ParseInput(strings.NewReader(payload)))
}

func gkManifest(t *testing.T, dir, run, status string) {
	t.Helper()
	body := fmt.Sprintf(`{"status":%q}`, status)
	if err := os.WriteFile(filepath.Join(dir, "workflows", run+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gkAdd(t *testing.T, g goal.Goal) goal.Goal {
	t.Helper()
	g.Session = "sess1"
	g, err := goal.Add(g)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func gkStatus(t *testing.T, id string) goal.Goal {
	t.Helper()
	g, err := goal.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A failing check blocks, changed output blocks again, the same output stalls, a passing check ticks done and the plan.
func TestGoalKeeperLifecycle(t *testing.T) {
	dir := gkEnv(t)
	plan := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(plan, []byte("# P\n\n## Tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(dir, "n")
	check := `n=$(cat ` + counter + ` 2>/dev/null || echo 0); echo "fail $n"; test -f ` + filepath.Join(dir, "ok") + ` && exit 0; exit 1`
	g := gkAdd(t, goal.Goal{Text: "ship", Check: check, CWD: dir, Plan: plan})

	if r := gkStop("sess1"); r.Decision != core.Block {
		t.Fatalf("first failing check: decision %v, want block", r.Decision)
	}
	_ = os.WriteFile(counter, []byte("1"), 0o644)
	if r := gkStop("sess1"); r.Decision != core.Block {
		t.Fatalf("changed output: decision %v, want block", r.Decision)
	}
	if r := gkStop("sess1"); r.Decision == core.Block || !strings.Contains(r.SystemMessage, "stalled") {
		t.Fatalf("same output: %+v, want a stalled note and no block", r)
	}
	if got := gkStatus(t, g.ID); got.Status != goal.Stalled {
		t.Fatalf("status = %q, want stalled", got.Status)
	}
	reopen := gkStatus(t, g.ID)
	reopen.Status, reopen.LastHash = goal.Open, ""
	_ = goal.Save(reopen)
	_ = os.WriteFile(filepath.Join(dir, "ok"), nil, 0o644)
	if r := gkStop("sess1"); r.Decision == core.Block {
		t.Fatalf("passing check blocked: %+v", r)
	}
	if got := gkStatus(t, g.ID); got.Status != goal.Done {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if data, _ := os.ReadFile(plan); !strings.Contains(string(data), "- [x] ship (task: "+g.ID+")") {
		t.Fatalf("plan line not ticked:\n%s", data)
	}
}

// Resuming a run closes the original goal instead of leaving it to stall on its killed manifest.
func TestWorkflowResumeClosesOriginalGoal(t *testing.T) {
	dir := gkEnv(t)
	gkLaunch(dir, "wf_aaa", `{}`)
	gkManifest(t, dir, "wf_aaa", "killed")
	if r := gkStop("sess1"); r.Decision != core.Block {
		t.Fatalf("killed run: decision %v, want block", r.Decision)
	}
	gkLaunch(dir, "wf_bbb", `{"resumeFromRunId":"wf_aaa"}`)
	r := gkStop("sess1")
	if r.Decision == core.Block || strings.Contains(r.SystemMessage, "stalled") {
		t.Fatalf("after resume: %+v, want no block and no stall", r)
	}
	for _, g := range goal.All() {
		if g.Run == "wf_aaa" && (g.Status != goal.Closed || g.Reason != "resumed as wf_bbb") {
			t.Errorf("wf_aaa goal = %s %q, want closed 'resumed as wf_bbb'", g.Status, g.Reason)
		}
	}
	if n := len(goal.All()); n != 2 {
		t.Errorf("%d goals, want 2 (no duplicate)", n)
	}
}

// Resuming under the same run id re-arms the goal: hash and blocks reset, still open, no duplicate.
func TestWorkflowResumeSameIDRearms(t *testing.T) {
	dir := gkEnv(t)
	gkLaunch(dir, "wf_aaa", `{}`)
	gkManifest(t, dir, "wf_aaa", "killed")
	gkStop("sess1")
	gkLaunch(dir, "wf_aaa", `{"resumeFromRunId":"wf_aaa"}`)
	all := goal.All()
	if len(all) != 1 || all[0].Status != goal.Open || all[0].LastHash != "" || all[0].Blocks != 0 {
		t.Fatalf("goals = %+v, want one open goal with hash and blocks reset", all)
	}
}

// A run that ended in any status but completed blocks the stop.
func TestWorkflowFailedRunBlocks(t *testing.T) {
	dir := gkEnv(t)
	gkLaunch(dir, "wf_ccc", `{}`)
	gkManifest(t, dir, "wf_ccc", "failed")
	r := gkStop("sess1")
	if r.Decision != core.Block || !strings.Contains(r.Reason, "run wf_ccc ended failed") {
		t.Fatalf("failed run: %+v, want a block naming 'ended failed'", r)
	}
}

// A run with no manifest is still running: pending, never blocking.
func TestWorkflowWithoutManifestIsPending(t *testing.T) {
	dir := gkEnv(t)
	gkLaunch(dir, "wf_ddd", `{}`)
	r := gkStop("sess1")
	if r.Decision == core.Block || !strings.Contains(r.SystemMessage, "pending") {
		t.Fatalf("no manifest: %+v, want pending and no block", r)
	}
}

// Another session's open goal is listed but never blocks this session's stop.
func TestGoalKeeperIgnoresOtherSessions(t *testing.T) {
	dir := gkEnv(t)
	g := gkAdd(t, goal.Goal{Text: "theirs", Check: "exit 1", CWD: dir})
	g.Session = "other"
	_ = goal.Save(g)
	r := gkStop("sess1")
	if r.Decision == core.Block || !strings.Contains(r.SystemMessage, "other session") {
		t.Fatalf("other session's goal: %+v, want a note and no block", r)
	}
}

// A hand-ticked plan line closes the goal without running its check.
func TestGoalKeeperHandTickedPlanCloses(t *testing.T) {
	dir := gkEnv(t)
	plan := filepath.Join(dir, "plan.md")
	_ = os.WriteFile(plan, []byte("## Tasks\n"), 0o644)
	g := gkAdd(t, goal.Goal{Text: "t", Check: "exit 1", CWD: dir, Plan: plan})
	data, _ := os.ReadFile(plan)
	_ = os.WriteFile(plan, []byte(strings.Replace(string(data), "- [ ]", "- [x]", 1)), 0o644)
	if r := gkStop("sess1"); r.Decision == core.Block {
		t.Fatalf("hand-ticked plan blocked: %+v", r)
	}
	if got := gkStatus(t, g.ID); got.Status != goal.Done {
		t.Fatalf("status = %q, want done", got.Status)
	}
}

// A goal that keeps failing with new output stalls once MaxBlocks stops were blocked.
func TestGoalKeeperMaxBlocksStalls(t *testing.T) {
	dir := gkEnv(t)
	g := gkAdd(t, goal.Goal{Text: "t", Check: "date +%s%N; exit 1", CWD: dir})
	for i := range goal.MaxBlocks {
		if r := gkStop("sess1"); r.Decision != core.Block {
			t.Fatalf("stop %d: decision %v, want block", i, r.Decision)
		}
	}
	if r := gkStop("sess1"); r.Decision == core.Block || !strings.Contains(r.SystemMessage, "stalled after") {
		t.Fatalf("past the cap: %+v, want a stalled note and no block", r)
	}
	if got := gkStatus(t, g.ID); got.Status != goal.Stalled {
		t.Fatalf("status = %q, want stalled", got.Status)
	}
}
