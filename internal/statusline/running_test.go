package statusline

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
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

const runningPayload = `{"model":{"display_name":"Opus"},"workspace":{"current_dir":"/tmp"}}`

// enableAgents turns on the agents segment (off by default) through the user config, keeping the other defaults.
func enableAgents(t *testing.T) {
	t.Helper()
	segs := maps.Clone(embeddedConfig().Segments) // the embedded map is shared: never mutate it
	segs["agents"] = true
	b, _ := json.Marshal(map[string]any{"segments": segs})
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ConfigDir(), "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The segment is always shown, at the floor of just the main thread, when nothing else runs.
// dashboard is excluded: its own columns list (not cfg.Order) omits "agents" by default.
func TestRunningSegmentAlwaysShownAtFloor(t *testing.T) {
	hermetic(t)
	enableAgents(t)
	for _, style := range []string{"gradient", "powerline", "capsule", "minimal"} {
		got := plain(Render([]byte(runningPayload), style))
		if !strings.Contains(got, " 1 ◉") {
			t.Errorf("%s: want floor '1 ◉', got %q", style, got)
		}
		if strings.Contains(got, "1 agents") {
			t.Errorf("%s: singular floor must not pluralize: %q", style, got)
		}
	}
}

// A single live, same-owner leg tips the count from 1 to 2, crossing the plural boundary.
func TestRunningSegmentCountsOwnLeg(t *testing.T) {
	hermetic(t)
	enableAgents(t)
	owner := inflight.SessionOwner()
	writeRunningDispatch(t, "20261024T000001.000-codex", os.Getpid(), owner)
	got := plain(Render([]byte(runningPayload), "gradient"))
	if !strings.Contains(got, " 2 ◉") {
		t.Errorf("want '2 ◉', got %q", got)
	}
}

// A dead leg (unreachable pid) never lifts the count off the floor.
func TestRunningSegmentIgnoresDeadLegs(t *testing.T) {
	hermetic(t)
	enableAgents(t)
	owner := inflight.SessionOwner()
	writeRunningDispatch(t, "20261024T000001.000-codex", 1<<22+12345, owner) // beyond pid_max: never alive
	got := plain(Render([]byte(runningPayload), "gradient"))
	if !strings.Contains(got, " 1 ◉") {
		t.Errorf("dead leg counted: %q", got)
	}
}

// A live leg owned by a different session, or carrying no owner at all, is not this session's to count.
func TestRunningSegmentFiltersByOwner(t *testing.T) {
	hermetic(t)
	enableAgents(t)
	owner := inflight.SessionOwner()
	if owner == 0 {
		t.Skip("inflight.SessionOwner() resolved to 0 in this test environment; owner filtering is untestable here")
	}
	writeRunningDispatch(t, "20261024T000001.000-other-owner", os.Getpid(), owner+999999)
	writeRunningDispatch(t, "20261024T000002.000-no-owner", os.Getpid(), 0)
	got := plain(Render([]byte(runningPayload), "gradient"))
	if !strings.Contains(got, " 1 ◉") {
		t.Errorf("a foreign-owner or ownerless leg was counted: %q", got)
	}
}
