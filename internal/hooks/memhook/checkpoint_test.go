package memhook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/memorytest"
	"github.com/edotau/claude-code/internal/paths"
)

func TestContextCheckpoint(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		t.Setenv(k, "")
	}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"assistant","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":%d,"cache_read_input_tokens":0,"output_tokens":0}}}` + "\n"
	payload := fmt.Sprintf(`{"session_id":"s1","transcript_path":%q}`, tr)
	_ = os.WriteFile(tr, fmt.Appendf(nil, line, 100_000), 0o600)
	calls := &memorytest.StubSpawn(t, true).Update
	contextCheckpoint(core.ParseInput(strings.NewReader(payload)))
	if calls.Load() != 0 {
		t.Fatalf("50%% must not spawn, got %d", calls.Load())
	}
	_ = os.WriteFile(tr, fmt.Appendf(nil, line, 180_000), 0o600)
	contextCheckpoint(core.ParseInput(strings.NewReader(payload)))
	if calls.Load() != 1 {
		t.Fatalf("90%% must spawn once, got %d", calls.Load())
	}
	contextCheckpoint(core.ParseInput(strings.NewReader(payload)))
	if calls.Load() != 1 {
		t.Errorf("second Stop in the same session must not spawn again, got %d", calls.Load())
	}
	t.Setenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "claude-sonnet-4-5[1m]")
	other := strings.Replace(payload, "s1", "s2", 1)
	contextCheckpoint(core.ParseInput(strings.NewReader(other)))
	if calls.Load() != 1 {
		t.Error("a [1m] session at 180K is 18% and must not spawn")
	}
	t.Setenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "")
	memorytest.StubSpawn(t, false)
	failed := strings.Replace(payload, "s1", "s3", 1)
	contextCheckpoint(core.ParseInput(strings.NewReader(failed)))
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "checkpoint", "s3")); err == nil {
		t.Error("a failed spawn must clear the once-per-session flag")
	}
}

// steward sets up a hermetic session whose transcript the returned func fills to the given token count.
func steward(t *testing.T) (payload func(extra string) *core.Input, fill func(tokens int), spawns *memory.Spawns) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(stewardPctEnv, "")
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		t.Setenv(k, "")
	}
	oO, oC, oL := sessionOwner, countOwned, liveDispatches
	sessionOwner = func() int { return 4242 }
	countOwned = func(int, time.Time) int { return 0 }
	liveDispatches = func(int) int { return 0 }
	t.Cleanup(func() { sessionOwner, countOwned, liveDispatches = oO, oC, oL })
	cwd, tr := t.TempDir(), filepath.Join(t.TempDir(), "t.jsonl")
	fill = func(tokens int) {
		line := `{"type":"assistant","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":%d,"output_tokens":0}}}` + "\n"
		if err := os.WriteFile(tr, fmt.Appendf(nil, line, tokens), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	payload = func(extra string) *core.Input {
		return core.ParseInput(strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"transcript_path":%q%s}`, cwd, tr, extra)))
	}
	return payload, fill, memorytest.StubSpawn(t, true)
}

func TestStopPromptsHandoffOnceWhenIdle(t *testing.T) {
	payload, fill, spawns := steward(t)
	fill(160_000) // 80% of 200K
	r := Stop(payload(""))
	if spawns.Update.Load() != 1 || !strings.Contains(r.Context, "write a handoff to") || !strings.Contains(r.Context, "80%") {
		t.Fatalf("idle 80%% must spawn and prompt, got spawns=%d ctx=%q", spawns.Update.Load(), r.Context)
	}
	if len(r.SystemMessage) > 80 || !strings.Contains(r.SystemMessage, "run /compact after") {
		t.Errorf("SystemMessage = %q", r.SystemMessage)
	}
	if r2 := Stop(payload("")); r2.Context != "" || spawns.Update.Load() != 1 {
		t.Errorf("second Stop in the same crossing must be silent, got %q spawns=%d", r2.Context, spawns.Update.Load())
	}
}

func TestStopDefersWhileWorkInFlight(t *testing.T) {
	for name, arrange := range map[string]func() string{
		"background task": func() string { return `,"background_tasks":[{"id":"b"}]` },
		"subagent marker": func() string { countOwned = func(int, time.Time) int { return 1 }; return "" },
		"live dispatch":   func() string { liveDispatches = func(int) int { return 1 }; return "" },
	} {
		t.Run(name, func(t *testing.T) {
			payload, fill, spawns := steward(t)
			fill(180_000)
			r := Stop(payload(arrange()))
			if spawns.Update.Load() != 0 || r.Context != "" {
				t.Fatalf("unsafe Stop must do nothing, got spawns=%d ctx=%q", spawns.Update.Load(), r.Context)
			}
			if _, err := os.Stat(filepath.Join(paths.StateDir(), "checkpoint", "s1")); err == nil {
				t.Error("unsafe Stop must write no flag")
			}
			countOwned = func(int, time.Time) int { return 0 }
			liveDispatches = func(int) int { return 0 }
			if r := Stop(payload("")); !strings.Contains(r.Context, "handoff") {
				t.Errorf("the next safe Stop must prompt, got %q", r.Context)
			}
		})
	}
}

func TestStopRearmsBelowThreshold(t *testing.T) {
	payload, fill, spawns := steward(t)
	fill(180_000)
	Stop(payload(""))
	fill(20_000) // after a compaction
	Stop(payload(""))
	fill(180_000)
	if r := Stop(payload("")); spawns.Update.Load() != 2 || !strings.Contains(r.Context, "handoff") {
		t.Fatalf("a new crossing must prompt again, got spawns=%d ctx=%q", spawns.Update.Load(), r.Context)
	}
}

func TestStewardThresholdEnv(t *testing.T) {
	for _, c := range []struct {
		env         string
		tokens      int
		spawns      int64
		wantContext bool
	}{
		{"", 140_000, 0, false},     // 70% < default 75
		{"60", 140_000, 1, true},    // lowered threshold
		{"junk", 140_000, 0, false}, // invalid -> 75
		{"0", 160_000, 1, false},    // update kept, prompt off
	} {
		t.Run(c.env, func(t *testing.T) {
			payload, fill, spawns := steward(t)
			t.Setenv(stewardPctEnv, c.env)
			fill(c.tokens)
			r := Stop(payload(""))
			if spawns.Update.Load() != c.spawns || (r.Context != "") != c.wantContext {
				t.Fatalf("spawns=%d ctx=%q", spawns.Update.Load(), r.Context)
			}
		})
	}
}

func TestStopHookActiveSkipsSteward(t *testing.T) {
	payload, fill, spawns := steward(t)
	fill(180_000)
	if r := Stop(payload(`,"stop_hook_active":true`)); spawns.Update.Load() != 0 || r.Context != "" {
		t.Fatalf("stop_hook_active must skip, got spawns=%d ctx=%q", spawns.Update.Load(), r.Context)
	}
}

// TestStopResolvesSessionRootOnce: the checkpoint and the update share one session-root read per Stop.
func TestStopResolvesSessionRootOnce(t *testing.T) {
	tr, payload := hookSession(t)
	writeEdits(t, tr, 5)
	reads := 0
	orig := sessionRoot
	sessionRoot = func(id, cwd string) string { reads++; return orig(id, cwd) }
	t.Cleanup(func() { sessionRoot = orig })
	Stop(payload())
	if reads != 1 {
		t.Fatalf("session root reads per Stop = %d, want 1", reads)
	}
}
