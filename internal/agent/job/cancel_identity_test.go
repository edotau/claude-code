package job

import (
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

func TestCancelRejectsReusedSplitReviewPID(t *testing.T) {
	for _, identity := range []string{"stale", "missing"} {
		t.Run(identity, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			old := executable
			executable = func() (string, error) { return sleeperScript(t), nil }
			t.Cleanup(func() { executable = old })
			rec := ClaimRecord("codex", agent.RunOptions{}, time.Now())
			rec.Progress = "part 1/2"
			if err := SpawnArgv(&rec, []string{"codex", "review", "--background"}, nil); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = kill(rec.PID) })
			if !sameStart(&rec) {
				t.Fatal("spawned review has no matching recorded process identity")
			}
			rec.PIDStart += "-stale"
			if identity == "missing" {
				rec.PIDStart = ""
			}
			if err := rec.Save(); err != nil {
				t.Fatal(err)
			}
			err := Cancel(&rec)
			if err == nil || !strings.Contains(err.Error(), "not the process spawned for it (pid reused); not signalling it") {
				t.Errorf("Cancel() = %v, want process-identity refusal", err)
			}
			assertReviewUnaffected(t, rec)
		})
	}
}

func assertReviewUnaffected(t *testing.T, rec Record) {
	t.Helper()
	if !rec.Alive() {
		t.Error("Cancel signalled an unrelated split review with matching command arguments")
	}
	saved, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "running" || saved.Error != "" {
		t.Errorf("Cancel rewrote unrelated review record: status=%q error=%q", saved.Status, saved.Error)
	}
}
