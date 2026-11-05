package memhook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
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
	calls := &StubSpawnForTest(t, true).Update
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
	spawnUpdate = func(string, string, string) bool { return false }
	failed := strings.Replace(payload, "s1", "s3", 1)
	contextCheckpoint(core.ParseInput(strings.NewReader(failed)))
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "checkpoint", "s3")); err == nil {
		t.Error("a failed spawn must clear the once-per-session flag")
	}
}
