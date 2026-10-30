package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/proc/inflight"
)

// spawnPayload is an Agent PreToolUse payload with an explicit tool_use_id and pinned model.
func spawnPayload(id string) string {
	return `{"tool_name":"Agent","tool_input":{"prompt":"x","model":"sonnet"},"tool_use_id":"` + id + `"}`
}

// postFailurePayload is the PostToolUseFailure shape: a bare top-level tool_use_id, no tool_name.
func postFailurePayload(id string) string { return `{"tool_use_id":"` + id + `"}` }

// markerNames returns only agent-* entries in the inflight dir, ignoring .lock and strays.
func markerNames(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(inflight.Dir())
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), inflight.Prefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

// dispatchEnv isolates the config dir and returns the cap one session may fill (min of account and total).
func dispatchEnv(t *testing.T) int {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if inflight.SessionOwner() == 0 {
		t.Skip("session owner unavailable")
	}
	lim := inflight.CurrentLimits()
	return min(lim.For(inflight.SessionAccount()), lim.Total)
}

func pre(payload string) Result { return dispatchPre(ParseInput(strings.NewReader(payload))) }

// Cap → claim → release spans the group: spawns fill the cap, the next blocks without a marker and names the
// numbers, a failed spawn's release frees exactly one slot, and a SubagentStop resolved through meta.json frees another.
func TestDispatchCapBlockReleaseAndStop(t *testing.T) {
	capN := dispatchEnv(t)
	for i := range capN {
		if r := pre(spawnPayload(fmt.Sprintf("u%d", i))); r.Decision == Block {
			t.Fatalf("spawn %d under the cap blocked: %s", i, r.Reason)
		}
	}
	r := pre(spawnPayload("over"))
	if r.Decision != Block || !strings.Contains(r.Reason, fmt.Sprintf("cap %d", capN)) ||
		!strings.Contains(r.Reason, "run this task sequentially") {
		t.Fatalf("spawn at the cap = %+v, want a Block naming the cap and the fix", r)
	}
	if n := len(markerNames(t)); n != capN {
		t.Fatalf("a blocked dispatch must claim nothing: %d markers, want %d", n, capN)
	}
	releaseSpawn(ParseInput(strings.NewReader(postFailurePayload("u0"))))
	if n := len(markerNames(t)); n != capN-1 {
		t.Fatalf("release of u0 left %d markers, want %d", n, capN-1)
	}

	tx := filepath.Join(t.TempDir(), "session.jsonl")
	sub := inflight.SubagentDir(tx)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "agent-a1.meta.json"), []byte(`{"toolUseId":"u1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := `{"hook_event_name":"SubagentStop","agent_id":"a1","transcript_path":"` + tx + `"}`
	subagentStopRunner(ParseInput(strings.NewReader(stop)))
	subagentStopRunner(ParseInput(strings.NewReader(stop))) // a second stop is a no-op
	if n := len(markerNames(t)); n != capN-2 {
		t.Fatalf("stop of a1 left %d markers, want %d", n, capN-2)
	}
	subagentStopRunner(ParseInput(strings.NewReader(`{"agent_id":"unknown","transcript_path":"` + tx + `"}`)))
	if n := len(markerNames(t)); n != capN-2 {
		t.Fatalf("a stop with no meta.json must retire nothing: %d markers", n)
	}
}

// A Workflow claims one slot per agent() site after the pin rewrite; the PostToolUse release frees them all.
func TestDispatchWorkflowClaimsOneSlotPerLeaf(t *testing.T) {
	capN := dispatchEnv(t)
	script := "agent('a', {model:'sonnet'});\nagent('b', {model:'sonnet'});\nawait agent('c');\n"
	payload := `{"tool_name":"Workflow","tool_use_id":"wf1","tool_input":{"script":` + fmt.Sprintf("%q", script) + `}}`
	if r := pre(payload); r.Decision == Block {
		t.Fatalf("workflow blocked: %s", r.Reason)
	}
	want := min(3, capN)
	if n := len(markerNames(t)); n != want {
		t.Fatalf("workflow claimed %d markers, want %d", n, want)
	}
	releaseSpawn(ParseInput(strings.NewReader(`{"tool_name":"Workflow","tool_use_id":"wf1"}`)))
	if n := len(markerNames(t)); n != 0 {
		t.Fatalf("release left %d markers", n)
	}
}

// Non-spawn tools neither count nor claim.
func TestDispatchIgnoresOtherTools(t *testing.T) {
	dispatchEnv(t)
	if r := pre(`{"tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"b"}`); r.Decision == Block || len(markerNames(t)) != 0 {
		t.Fatalf("a Bash call claimed or blocked: %+v %v", r, markerNames(t))
	}
}

// A pin-step Block (bad workflow pin) returns as-is and claims nothing.
func TestDispatchPinBlockClaimsNothing(t *testing.T) {
	dispatchEnv(t)
	payload := `{"tool_name":"Workflow","tool_use_id":"bad","tool_input":{"script":"agent('x', {model:'gpt-4'});"}}`
	if r := pre(payload); r.Decision != Block {
		t.Fatalf("a bad pin must block, got %+v", r)
	}
	if n := len(markerNames(t)); n != 0 {
		t.Fatalf("a pin Block wrote %d markers", n)
	}
}
