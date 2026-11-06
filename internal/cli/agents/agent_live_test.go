package agents

import (
	"errors"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// A holder that never became ready leaves its record marked error with the cause, not running.
func TestAbandonLiveMarksRecordFailed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "hi"}, time.Now())
	rec.Status = "running"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	abandonLive(&rec, errors.New("socket never appeared"))
	got, err := agent.LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || got.Error != "socket never appeared" {
		t.Errorf("record = status %q error %q, want error / cause", got.Status, got.Error)
	}
}

// agents tree --watch rejects a zero or negative --interval instead of busy-looping.
func TestCmdAgentsTreeWatchRejectsNonPositiveInterval(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, iv := range []string{"0", "-1s"} {
		code, _, stderr := captureOutput(t, func() int { return CmdAgents([]string{"tree", "--watch", "--interval", iv}) })
		if code != 2 || stderr == "" {
			t.Errorf("--interval %s: exit %d stderr %q, want exit 2 with a message", iv, code, stderr)
		}
	}
}
