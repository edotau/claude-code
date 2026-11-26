package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/health"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
)

// A retry hint or an appended path leaves the attempt key alone; another plan task or text changes it.
func TestAttemptKeyIgnoresHarnessAdditions(t *testing.T) {
	task := TaskKey{Text: "fix the parser\n\nTask: plan-a 1.2", PlanTask: "plan-a 1.2"}
	hint := assets.AgentRecovery(assets.RecoveryFrame{Prompt: task.Text, ID: "d-1", Status: "error", Log: "log"})
	same := []string{task.Text + "\n\n<memory-recall>\n- x", hint, task.Text + packMark + "/tmp/pack.md"}
	for _, text := range same {
		if attemptKey(TaskKey{Text: text, PlanTask: task.PlanTask}) != attemptKey(task) {
			t.Errorf("key changed for %q", text[len(task.Text):])
		}
	}
	if attemptKey(TaskKey{Text: task.Text, PlanTask: "plan-a 1.3"}) == attemptKey(task) || attemptKey(TaskKey{Text: "other"}) == attemptKey(task) {
		t.Error("a different plan task or text must be a different key")
	}
}

// Two failures degrade the task, a success clears it, and the notice replaces the resend line.
func TestTwoFailedAttemptsDegradeAndSuccessClears(t *testing.T) {
	setConfigDir(t)
	rec := Record{ID: "d-1", Status: "error", Request: RunOptions{Prompt: "task"}, PlanTask: "p 1"}
	rec.NoteHealth(nil)
	if h := DispatchHealth(TaskKey{Text: "task", PlanTask: "p 1"}); h.Degraded || h.Attempts != 1 {
		t.Fatalf("after one failure: %+v", h)
	}
	if got := ResumeHint(rec); !strings.Contains(got, "--retry") {
		t.Fatalf("first failure keeps the resend line: %q", got)
	}
	rec.NoteHealth(nil)
	wantTwoFailed(t, rec)
	rec.Status = "ok"
	rec.NoteHealth(nil)
	if h := DispatchHealth(TaskKey{Text: "task", PlanTask: "p 1"}); h.Degraded || h.Attempts != 0 {
		t.Fatalf("after a success: %+v", h)
	}
}

// The proxy's refusal of its own secret is dispatch-down, whatever task asks next; a success ends it.
func TestProxyRefusedIsDispatchDown(t *testing.T) {
	setConfigDir(t)
	degradeOn(&retry.Error{Kind: retry.KindAuth, ProxyRefused: true})
	if h := DispatchHealth(TaskKey{Text: "any"}); !h.Degraded || h.Reason != health.RouterSecret {
		t.Fatalf("health %+v, want router_secret", h)
	}
	Record{Status: "ok", Request: RunOptions{Prompt: "any"}}.NoteHealth(nil)
	if h := DispatchHealth(TaskKey{Text: "any"}); !h.Degraded {
		t.Fatalf("a run that skipped the proxy cleared router_secret: %+v", h)
	}
	health.ClearDown() // an unauthenticated probe does not prove the secret
	if h := DispatchHealth(TaskKey{Text: "any"}); !h.Degraded {
		t.Fatalf("a liveness probe cleared router_secret: %+v", h)
	}
	Record{Status: "ok", Proxy: "scoped", Request: RunOptions{Prompt: "any"}}.NoteHealth(nil)
	if h := DispatchHealth(TaskKey{Text: "any"}); h.Degraded {
		t.Fatalf("a successful record must clear dispatch-down: %+v", h)
	}
}

// An unreadable health file reads as healthy with no attempts and never fails the caller.
func TestDispatchHealthUnreadableFile(t *testing.T) {
	setConfigDir(t)
	p := filepath.Join(paths.StateDir(), "agent", "health.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil || os.WriteFile(p, []byte("{not json"), 0o600) != nil {
		t.Fatal("cannot seed the file")
	}
	if h := DispatchHealth(TaskKey{Text: "x"}); h != (Health{}) {
		t.Fatalf("unreadable file read as %+v, want zero", h)
	}
}

func wantTwoFailed(t *testing.T, rec Record) {
	t.Helper()
	h := DispatchHealth(TaskKey{Text: "task\n\nAn earlier attempt (dispatch d-1", PlanTask: "p 1"})
	if !h.Degraded || h.Reason != health.TwoFailed || h.Attempts != 2 || h.Since.IsZero() {
		t.Fatalf("after two failures: %+v", h)
	}
	if got, want := ResumeHint(rec), "dispatch degraded: two_failed_attempts; continue in the main session"; got != want {
		t.Fatalf("ResumeHint = %q, want %q", got, want)
	}
}

// A cancelled run is no failed attempt, a lost run is one, and the failure is counted before the record is visible.
func TestNoteHealthCancelledAndLost(t *testing.T) {
	setConfigDir(t)
	rec := Record{Status: "error", Request: RunOptions{Prompt: "t"}}
	for range 2 {
		rec.NoteHealth(&retry.Error{Kind: retry.KindFatal, Cancelled: true, Err: os.ErrClosed})
	}
	if h := DispatchHealth(TaskKey{Text: "t"}); h.Attempts != 0 {
		t.Fatalf("cancelled runs counted: %+v", h)
	}
	Record{Status: "lost", Request: RunOptions{Prompt: "t"}}.NoteHealth(nil)
	if h := DispatchHealth(TaskKey{Text: "t"}); h.Attempts != 1 {
		t.Fatalf("a lost run must count once: %+v", h)
	}
}
