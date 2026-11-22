package tree

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/serve"
)

// A record still marked running whose process is dead reads "lost" like the dispatches list, never "quiet".
func TestDeadRunningDispatchReadsLost(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("codex", agent.RunOptions{}, now.Add(-20*time.Minute))
	rec.Status, rec.PID = "running", deadPID
	d := dispatchFromRecord(rec, "", now)
	if row := dispatchRow(d, now); d.Quiet || d.Status != "lost" || strings.Contains(row, "quiet") || !strings.Contains(row, "lost") {
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
	if got := sinceCell(dispatchFromRecord(rec, "", now), now); got != "0s" {
		t.Errorf("since = %q, want 0s", got)
	}
}

// A running dispatch's last sign of life is its newest event/log write; past quietAfter its since cell reads "⚠7m".
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
	}{{-30 * time.Second, false, "30s"}, {-7 * time.Minute, true, "⚠7m"}} {
		if err := os.Chtimes(rec.EventsPath(), now.Add(c.wrote), now.Add(c.wrote)); err != nil {
			t.Fatal(err)
		}
		d := dispatchFromRecord(rec, "", now)
		if got := sinceCell(d, now); d.Quiet != c.quiet || d.SignalSource != "events" || got != c.want {
			t.Errorf("wrote %v ago: quiet=%v source=%q since=%q, want quiet=%v with %q", c.wrote, d.Quiet, d.SignalSource, got, c.quiet, c.want)
		}
	}
}

// dispatchRow is a dispatch's cells joined, for substring checks.
func dispatchRow(d Dispatch, now time.Time) string { return strings.Join(jobCells(d, now, nil), " ") }

// sinceCell is a dispatch row's time-since-last-event cell.
func sinceCell(d Dispatch, now time.Time) string { return jobCells(d, now, nil)[4] }

// A running row shows agent, model · route, since and elapsed, and never the brief's header or task.
func TestRunningRowHasNoTask(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("claude", agent.RunOptions{Prompt: "Owned: [\"a.go\"]\nTask:\nfix the bug", Lead: "qa"}, now.Add(-time.Minute))
	rec.Status, rec.PID, rec.Model = "running", os.Getpid(), "databricks-claude-sonnet-5-5"
	if err := os.WriteFile(rec.EventsPath(), []byte(`{"type":"tool_use"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	renderTree(&b, &Tree{Dispatches: []Dispatch{dispatchFromRecord(rec, "", now)}}, now, 80, "")
	row := strings.Split(b.String(), "\n")[1]
	want := []string{"●", "claude", "claude-sonnet-5-5", "·", "qa", "↑-", "↓-", "0s", "1m"}
	if got := strings.Fields(row); !slices.Equal(got, want) {
		t.Errorf("row = %q, want fields %q", row, want)
	}
}

// A job waiting for serve admission shows "○" and its queue wait and pool in place of tokens, never quiet.
func TestQueuedRowShowsWait(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("codex", agent.RunOptions{}, now.Add(-20*time.Minute))
	rec.Status, rec.PID = "running", os.Getpid()
	rec.Progress = "queued " + now.Add(-70*time.Second).UTC().Format(time.RFC3339Nano) + " databricks:qa"
	d := dispatchFromRecord(rec, "", now)
	pools := map[string]serve.PoolRow{"databricks:qa": {Pool: "databricks:qa", Running: 2, Cap: 4}}
	if cells := jobCells(d, now, pools); d.Quiet || jobMarker(d) != "○" || cells[2] != "queued 1m10s (pool databricks:qa 2/4)" {
		t.Errorf("queued: quiet=%v marker=%q cells=%q", d.Quiet, jobMarker(d), cells)
	}
}
