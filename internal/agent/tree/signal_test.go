package tree

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// A record still marked running whose process is dead reads "lost" like the dispatches list, never "quiet".
func TestDeadRunningDispatchReadsLost(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("codex", agent.RunOptions{}, now.Add(-20*time.Minute))
	rec.Status, rec.PID = "running", deadPID
	d := dispatchFromRecord(rec, "", now)
	if row := formatDispatch(d, now); d.Quiet || d.Status != "lost" || strings.Contains(row, "quiet") || !strings.Contains(row, "lost") {
		t.Errorf("dead running dispatch: quiet=%v status=%q row=%q, want lost", d.Quiet, d.Status, row)
	}
}

// A last signal in the future (clock moved back) shows a zero age, never a negative one.
func TestFutureSignalClampsToZero(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("codex", agent.RunOptions{}, now.Add(-time.Minute))
	rec.Status, rec.PID = "running", os.Getpid()
	if err := os.WriteFile(rec.EventsPath(), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := now.Add(time.Hour)
	if err := os.Chtimes(rec.EventsPath(), future, future); err != nil {
		t.Fatal(err)
	}
	if row := formatDispatch(dispatchFromRecord(rec, "", now), now); !strings.Contains(row, "last=0s") {
		t.Errorf("row = %q, want last=0s", row)
	}
}

// A running dispatch's last sign of life is its newest event/log write; past quietAfter the row reads "⚠ quiet".
func TestRunningDispatchSignalAndQuiet(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("codex", agent.RunOptions{}, now.Add(-20*time.Minute))
	rec.Status, rec.PID = "running", os.Getpid()
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
