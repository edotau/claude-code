package job

import (
	"os"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
)

func TestRecordSavePersistsOwnProcessIdentity(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := ClaimRecord("codex", agent.RunOptions{}, time.Now())
	rec.PID = os.Getpid()
	want, err := proc.StartTime(rec.PID)
	if err != nil {
		t.Fatal(err)
	}
	if want == "" {
		t.Fatal("current process has no start time")
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.PIDStart != want {
		t.Fatalf("reloaded PIDStart = %q, want current process start time %q", saved.PIDStart, want)
	}
	if !sameStart(&saved) {
		t.Fatal("Cancel's process-identity check rejected the reloaded job's own process")
	}
}
