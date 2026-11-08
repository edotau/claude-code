package ask

import (
	"flag"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Given a background spawn that fails (the test binary is never relaunched), Then the saved record leaves running.
func TestAskSpawnFailureFinishesRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := job.ClaimRecord("claude", agent.RunOptions{}, time.Now())
	if code := askSpawn(flag.NewFlagSet("ask", flag.ContinueOnError), &client.Prepared{Record: rec}, false, helptext.Text, "claude"); code != 1 {
		t.Fatalf("askSpawn = %d, want 1", code)
	}
	got, err := job.LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || got.Error == "" {
		t.Errorf("record = %q/%q, want error with the cause", got.Status, got.Error)
	}
}
