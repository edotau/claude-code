package hooks

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
)

const deadJob = "20261116T110000.000-opencode"

// watchStubs fixes the clock, owner, classifier output and resume launch; it returns the call counters.
func watchStubs(t *testing.T, stale ...agent.StaleJob) (now *time.Time, checks, resumes *int) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(agent.WatchEnv, "")
	clock, nc, nr := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC), 0, 0
	oldNow, oldOwner, oldJobs, oldStart := watchNow, watchOwner, watchJobs, startResume
	watchNow = func() time.Time { return clock }
	watchOwner = func() int { return 77 }
	watchJobs = func(int, time.Time, agent.WatchLimits) (agent.WatchReport, error) {
		nc++
		return agent.WatchReport{Stale: stale}, nil
	}
	startResume = func([]string) (string, error) { nr++; return "20261116T120000.000-opencode", nil }
	t.Cleanup(func() { watchNow, watchOwner, watchJobs, startResume = oldNow, oldOwner, oldJobs, oldStart })
	return &clock, &nc, &nr
}

func promptIn(session string) *core.Input {
	return core.ParseInput(strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"` + session +
		`","prompt":"hi"}`))
}

func deadStale() agent.StaleJob {
	return agent.StaleJob{ID: deadJob, Task: "dead-leaf", Agent: "opencode", Reason: agent.WatchDead, SessionID: "ses-1",
		Next: []string{"claude-code", "agents", "ask", "--agent", "opencode", "--session", "ses-1", "--background", "go"}}
}

func TestWatchTickThrottlesAndReportsOncePerReason(t *testing.T) {
	silent := agent.StaleJob{ID: "20261116T100000.000-codex", Task: "slow-leaf", Agent: "codex", Reason: agent.WatchSilent,
		Next: []string{"claude-code", "agents", "result", "20261116T100000.000-codex"}}
	now, checks, _ := watchStubs(t, silent)
	if got := watchTick(promptIn("s1")); !strings.Contains(got.Context, "slow-leaf") || *checks != 1 {
		t.Fatalf("first tick: context %q checks %d, want the job line and one check", got.Context, *checks)
	}
	*now = now.Add(30 * time.Second)
	if got := watchTick(promptIn("s1")); got.Context != "" || *checks != 1 {
		t.Fatalf("tick inside 60s: context %q checks %d, want throttled", got.Context, *checks)
	}
	*now = now.Add(31 * time.Second)
	if got := watchTick(promptIn("s1")); got.Context != "" || *checks != 2 {
		t.Fatalf("tick after 60s: context %q checks %d, want a check and no repeat line", got.Context, *checks)
	}
	if got := watchTick(promptIn("s2")); !strings.Contains(got.Context, "slow-leaf") {
		t.Fatalf("another session: context %q, want its own first report", got.Context)
	}
}

// The runner and the tick run together; their notes join runner first, as when they ran one after the other.
func TestWatchedJoinsRunnerThenTick(t *testing.T) {
	silent := agent.StaleJob{ID: "20261116T100000.000-codex", Task: "slow-leaf", Agent: "codex", Reason: agent.WatchSilent,
		Next: []string{"claude-code", "agents", "result", "20261116T100000.000-codex"}}
	watchStubs(t, silent)
	runner := func(*core.Input) core.Result {
		return core.Result{Context: "runner note", SystemMessage: "sm", Stderr: "err"}
	}
	got := watched(runner, promptIn("s1"))
	if !strings.HasPrefix(got.Context, "runner note") || !strings.Contains(got.Context, "slow-leaf") ||
		!strings.HasPrefix(got.SystemMessage, "sm; watch: 1 stale") || got.Stderr != "err" {
		t.Fatalf("watched = %+v, want runner text first, then the tick's line and message", got)
	}
}

func TestWatchedRaisesARunnerPanicUnchanged(t *testing.T) {
	watchStubs(t)
	defer func() {
		if got := recover(); got != "runner boom" {
			t.Fatalf("recovered %#v, want the runner's own panic value", got)
		}
	}()
	watched(func(*core.Input) core.Result { panic("runner boom") }, promptIn("s1"))
	t.Fatal("watched returned after a runner panic")
}

func TestWatchTickResumesADeadJobOnce(t *testing.T) {
	now, _, resumes := watchStubs(t, deadStale())
	got := watchTick(promptIn("s1"))
	if *resumes != 1 || !strings.Contains(got.Context, "resumed once as 20261116T120000.000-opencode") {
		t.Fatalf("resumes %d context %q, want one resume named in the line", *resumes, got.Context)
	}
	if id, ok := agent.ResumedAs(deadJob); !ok || id != "20261116T120000.000-opencode" {
		t.Fatalf("resume marker %q %v, want the new job id", id, ok)
	}
	*now = now.Add(2 * time.Minute)
	watchTick(promptIn("s2")) // a fresh session would report it again, but never resume it twice
	if *resumes != 1 {
		t.Fatalf("resumes %d after a second report, want 1", *resumes)
	}
}

func TestWatchTickModes(t *testing.T) {
	_, checks, resumes := watchStubs(t, deadStale())
	t.Setenv(agent.WatchEnv, "OFF")
	if got := watchTick(promptIn("s1")); got.Context != "" || *checks != 0 {
		t.Fatalf("HARNESS_WATCH=off: context %q checks %d, want nothing", got.Context, *checks)
	}
	t.Setenv(agent.WatchEnv, "dry")
	if got := watchTick(promptIn("s1")); *resumes != 0 || !strings.Contains(got.Context, "would resume once") {
		t.Fatalf("dry: resumes %d context %q, want the command and no launch", *resumes, got.Context)
	}
}

func TestMarkCollectedOnlyEndedJobs(t *testing.T) {
	watchStubs(t)
	for id, status := range map[string]string{"20261116T100000.000-codex": "ok", "20261116T100100.000-codex": "running"} {
		if err := (agent.Record{ID: id, Agent: "codex", Status: status}).Save(); err != nil {
			t.Fatal(err)
		}
		in := core.ParseInput(strings.NewReader(`{"hook_event_name":"PostToolUse","tool_name":"Bash",` +
			`"tool_input":{"command":"claude-code agents result ` + id + `"},"tool_response":{"stdout":"done"}}`))
		if err := markCollected(in); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(agent.WatchMarker("collected", id))
		if (err == nil) != (status == "ok") {
			t.Fatalf("%s job: collected marker present=%v, want %v", status, err == nil, status == "ok")
		}
	}
}

func TestRunningLine(t *testing.T) {
	now := time.Date(2026, 11, 19, 20, 10, 0, 0, time.UTC)
	rec := func(id, ag, task string, age time.Duration) agent.Record {
		return agent.Record{ID: id, Agent: ag, PlanTask: task, Time: now.Add(-age)}
	}
	r1, r2 := rec("j1", "codex", "Phase 2.2", 6*time.Minute), rec("j2", "opencode", "", 40*time.Second)
	many := []agent.Record{r1, r2, rec("j3", "a", "", time.Minute), rec("j4", "a", "", time.Minute), rec("j5", "a", "", time.Minute)}
	for _, c := range []struct {
		name    string
		running []agent.Record
		stale   []agent.StaleJob
		want    string
	}{
		{"none", nil, nil, ""},
		{"entries", []agent.Record{r1, r2}, nil, "running: Task Phase 2.2 (codex · 6m00s · ⚠ no event 6m00s); Job j2 (opencode · 40s · last event 40s ago)"},
		{"cap", many, nil, "running: Task Phase 2.2 (codex · 6m00s · ⚠ no event 6m00s); Job j2 (opencode · 40s · last event 40s ago); Job j3 (a · 1m00s · no live signal from a); +2 more"},
		{"stale excluded", []agent.Record{r1, r2}, []agent.StaleJob{{ID: "j1"}}, "running: Job j2 (opencode · 40s · last event 40s ago)"},
	} {
		if got := runningLine(c.running, c.stale, now); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
