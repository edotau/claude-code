package tree

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// A running dispatch's last sign of life is its newest event/log write; past quietAfter the row reads "⚠ quiet".
func TestRunningDispatchSignalAndQuiet(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("codex", agent.RunOptions{}, now.Add(-20*time.Minute))
	rec.Status = "running"
	if err := os.WriteFile(rec.EventsPath(), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		wrote time.Duration
		quiet bool
		want  string
	}{{-30 * time.Second, false, "last=30s"}, {-7 * time.Minute, true, "⚠ quiet 7m"}} {
		if err := os.Chtimes(rec.EventsPath(), now.Add(c.wrote), now.Add(c.wrote)); err != nil {
			t.Fatal(err)
		}
		d := dispatchFromRecord(rec, "", now)
		if d.Quiet != c.quiet || d.SignalSource != "events" || !strings.Contains(formatDispatch(d, now), c.want) {
			t.Errorf("wrote %v ago: quiet=%v source=%q row=%q, want quiet=%v with %q", c.wrote, d.Quiet, d.SignalSource, formatDispatch(d, now), c.quiet, c.want)
		}
	}
}
