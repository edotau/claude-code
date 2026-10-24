package statusline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func writeRunningDispatch(t *testing.T, id string, pid int) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "dispatch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"id": id, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"agent": "codex", "request": map[string]any{}, "status": "running", "pid": pid})
	if err := os.WriteFile(filepath.Join(dir, id+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

const runningPayload = `{"model":{"display_name":"Opus"},"workspace":{"current_dir":"/tmp"}}`

// The counter stays hidden at N == 0 in every style, so the existing goldens are unaffected.
func TestRunningSegmentHiddenWhenNothingRuns(t *testing.T) {
	hermetic(t)
	for _, style := range []string{"gradient", "powerline", "capsule", "minimal", "dashboard"} {
		if got := plain(Render([]byte(runningPayload), style)); strings.Contains(got, "running") {
			t.Errorf("%s: segment shown with nothing running: %q", style, got)
		}
	}
}

// A live dispatch shows as "1 running (0 agents · 1 legs)"; a dead one is not counted.
func TestRunningSegmentCountsLiveLegsOnly(t *testing.T) {
	hermetic(t)
	writeRunningDispatch(t, "20261024T000001.000-codex", os.Getpid())
	got := plain(Render([]byte(runningPayload), "gradient"))
	if !strings.Contains(got, "1 running") || !strings.Contains(got, "0 agents") || !strings.Contains(got, "1 legs") {
		t.Errorf("want '1 running (0 agents · 1 legs)', got %q", got)
	}
}

func TestRunningSegmentIgnoresDeadLegs(t *testing.T) {
	hermetic(t)
	writeRunningDispatch(t, "20261024T000001.000-codex", 1<<22+12345) // beyond pid_max: never alive
	if got := plain(Render([]byte(runningPayload), "gradient")); strings.Contains(got, "running") {
		t.Errorf("dead dispatch counted: %q", got)
	}
}
