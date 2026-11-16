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
