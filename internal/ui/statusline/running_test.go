package statusline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
)

func writeRunningDispatch(t *testing.T, id string, pid, owner int) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "dispatch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"id": id, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"agent": "codex", "request": map[string]any{}, "status": "running", "pid": pid, "owner": owner})
	if err := os.WriteFile(filepath.Join(dir, id+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// runningDispatches counts only this session's live dispatches: dead pids and other owners stay out.
func TestRunningCountsOwnLiveDispatchesOnly(t *testing.T) {
	cases := []struct {
		name  string
		pid   int
		owner func(int) int
		want  int
	}{
		{"own live", os.Getpid(), func(o int) int { return o }, 1},
		{"dead pid", 1<<22 + 12345, func(o int) int { return o }, 0}, // beyond pid_max: never alive
		{"other owner", os.Getpid(), func(o int) int { return o + 999999 }, 0},
		{"no owner", os.Getpid(), func(int) int { return 0 }, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hermetic(t)
			owner := inflight.SessionOwner()
			if owner == 0 && c.name != "own live" && c.name != "dead pid" {
				t.Skip("inflight.SessionOwner() is 0 here; owner filtering is untestable")
			}
			writeRunningDispatch(t, "20261024T000001.000-codex", c.pid, c.owner(owner))
			if got := runningDispatches(Input{}); got != c.want {
				t.Errorf("dispatches = %d, want %d", got, c.want)
			}
		})
	}
}

// total gains a trailing "+" only while one of this session's dispatch dispatches runs.
func TestTotalSegmentPlusWhileDispatchRuns(t *testing.T) {
	hermetic(t)
	var in Input
	if err := json.Unmarshal([]byte(`{"session_id":"s1","context_window":{"total_input_tokens":812}}`), &in); err != nil {
		t.Fatal(err)
	}
	if got := totalSegment(in); got != "812" {
		t.Fatalf("idle = %q, want 812", got)
	}
	writeRunningDispatch(t, "20261024T000002.000-codex", os.Getpid(), inflight.SessionOwner())
	// runningDispatches caches for runningTTL, so drop the stamp the idle call wrote.
	_ = os.Remove(stampFile("running", paths.StateDir()+"\x00"+in.TranscriptPath))
	if got := totalSegment(in); got != "812+" {
		t.Errorf("running = %q, want 812+", got)
	}
}
