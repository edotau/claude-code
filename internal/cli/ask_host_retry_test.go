package cli

import (
	"encoding/json"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

// ask --retry of a --host dispatch replays it on the same host through the same remote binary, never locally.
func TestAskRetryOfHostDispatchRunsRemote(t *testing.T) {
	lines := ndjson(t, resultEvent(agent.Envelope{Status: agent.StatusOK, Agent: "claude", Answer: "ok"}))
	bin := hostFixture(t, "", lines, 0)
	calls := recordingRemote(t, bin)
	code, out := captureAskStdout(t, "--host", "fakehost", "--remote-bin", bin, "--format", "json", "go")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var env agent.Envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.DispatchID == "" {
		t.Fatalf("no dispatch id (%v):\n%s", err, out)
	}
	if code, out = captureAskStdout(t, "--retry", env.DispatchID, "--format", "json"); code != 0 {
		t.Fatalf("retry exit %d:\n%s", code, out)
	}
	if got := readCalls(t, calls); len(got) != 2 {
		t.Fatalf("remote ran %d times, want 2 (the retry ran locally): %q", len(got), got)
	}
}
