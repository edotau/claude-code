package tree

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/dev/contract"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/routing/inflight"
)

type reapWriteFunc func([]byte) (int, error)

func (f reapWriteFunc) Write(p []byte) (int, error) { return f(p) }

// reapSocketConfig uses a short /tmp root: a macOS unix socket path caps at 104 bytes.
func reapSocketConfig(t *testing.T) {
	hosttest.SkipOnWindows(t, "/tmp socket dir")
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
}

func saveReapRecord(t *testing.T, pid int, at time.Time) agent.Record {
	t.Helper()
	rec := agent.ClaimRecord("claude", agent.RunOptions{}, at)
	rec.Live, rec.PID = true, pid
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestReapKeepsSocketCreatedAfterRecordSnapshot(t *testing.T) {
	reapSocketConfig(t)
	now := time.Now()
	saveReapRecord(t, deadPID, now.Add(-time.Minute))
	var socket string
	out := reapWriteFunc(func(p []byte) (int, error) {
		if strings.HasPrefix(string(p), "reaped dispatch ") {
			rec := saveReapRecord(t, os.Getpid(), now)
			socket = agent.LiveSocket(rec.ID)
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := ln.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		return len(p), nil
	})
	var errw bytes.Buffer
	if code := Reap(false, out, &errw); code != 0 {
		t.Fatalf("Reap = %d: %s", code, &errw)
	}
	if socket == "" {
		t.Fatal("dispatch output did not trigger the post-snapshot session start")
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("new live session's socket was removed after the record snapshot: %v", err)
	}
}

func TestReapReportsRemovalFailures(t *testing.T) {
	for _, kind := range []string{"socket", "marker"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			path := agent.LiveSocket("orphan")
			if kind == "marker" {
				path = filepath.Join(inflight.Dir(), "agent-2147483646-stale")
			}
			// A nonempty entry makes os.Remove fail even when the tests run as root.
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "child"), []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errw bytes.Buffer
			if code := Reap(false, &out, &errw); code != 1 {
				t.Fatalf("Reap = %d, want 1 for failed %s removal; stderr=%s", code, kind, &errw)
			}
			if !strings.Contains(errw.String(), kind+" "+filepath.Base(path)) {
				t.Errorf("stderr = %q, want failed %s name", errw.String(), kind)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("failed removal must leave the entry in place: %v", err)
			}
		})
	}
}

func TestDropDispatchSessionsKeepsConflictingSessionID(t *testing.T) {
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	dispatch := Dispatch{
		Agent: "claude", sessionID: "session-a", cwd: "/repo",
		started: start, prompt: "Summarize the retry logic",
	}
	sessions := []Session{
		{ID: "session-a", Cwd: dispatch.cwd, Title: dispatch.prompt, Updated: start.Add(time.Minute)},
		{ID: "session-b", Cwd: dispatch.cwd, Title: dispatch.prompt, Updated: start.Add(time.Minute)},
	}
	got := dropDispatchSessions(sessions, []Dispatch{dispatch})
	if len(got) != 1 || got[0].ID != "session-b" {
		t.Fatalf("sessions = %+v, want unrelated session-b retained despite matching title", got)
	}
}

func TestReapReportsMarkerDirectoryStatFailure(t *testing.T) {
	hosttest.SkipOnWindows(t, "symlinks")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := inflight.Dir()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(dir), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil || os.IsNotExist(err) {
		t.Fatalf("stat self-referential marker directory = %v, want non-missing error", err)
	}
	var out, errw bytes.Buffer
	if code := Reap(false, &out, &errw); code != 1 {
		t.Fatalf("Reap = %d, want 1 for marker directory stat failure; stdout=%q stderr=%q", code, &out, &errw)
	}
	if !strings.Contains(errw.String(), "marker dir:") {
		t.Fatalf("stderr = %q, want marker directory error", &errw)
	}
}

// A holder lost past its own attempt deadline is a budget end: no re-send, before or after the reap marks it (A4).
func TestResendHolderSkipsHolderPastDeadline(t *testing.T) {
	reapSocketConfig(t)
	rec := saveReapRecord(t, deadPID, time.Now().Add(-2*agent.AttemptTimeoutFor(agent.RunOptions{})))
	if got := resendHolder(contract.Contract{Holders: []string{rec.ID}}); got != "" {
		t.Fatalf("resent a holder past its deadline as %q", got)
	}
	if n, code := reapRecords([]agent.Record{rec}, false, "reaped", &bytes.Buffer{}, &bytes.Buffer{}); n != 1 || code != 0 {
		t.Fatalf("reap: n=%d code=%d", n, code)
	}
	reaped, err := agent.LoadRecord(rec.ID)
	if err != nil || reaped.BudgetKind != "deadline" || !strings.HasPrefix(agent.ResumeHint(reaped), "Budget end:") {
		t.Fatalf("reaped past its deadline without a budget end: %v %+v", err, reaped)
	}
	if got := resendHolder(contract.Contract{Holders: []string{rec.ID}}); got != "" {
		t.Fatalf("resent a reaped holder past its deadline as %q", got)
	}
}

// A run that saved a budget-ended result before its process left keeps that kind when reaped, however short it ran.
func TestReapKeepsSavedBudgetKind(t *testing.T) {
	reapSocketConfig(t)
	rec := saveReapRecord(t, deadPID, time.Now().Add(-time.Minute))
	env := wire.NewEnvelope(wire.Result{Agent: "claude", DurationMs: time.Second.Milliseconds()}, retry.WithBudget(errors.New("quiet"), retry.BudgetIdle), rec.ID)
	events := wire.NewEventLog(rec.EventsPath(), rec.ID)
	_ = events.Emit(wire.Event{Type: wire.EventStart})
	_ = events.Emit(wire.ResultEvent(env))
	events.Close()
	reapRecords([]agent.Record{rec}, false, "reaped", &bytes.Buffer{}, &bytes.Buffer{})
	got, err := agent.LoadRecord(rec.ID)
	if err != nil || got.BudgetKind != "idle" || resendHolder(contract.Contract{Holders: []string{rec.ID}}) != "" {
		t.Fatalf("saved budget kind lost on reap: %v %+v", err, got)
	}
}

// A holder the user cancelled is terminal (never re-sent); a lost one is re-sent once.
func TestResendHolderSkipsCancelledHolder(t *testing.T) {
	reapSocketConfig(t)
	calls := 0
	old := resend
	resend = func(context.Context, string) (string, error) { calls++; return "next", nil }
	t.Cleanup(func() { resend = old })
	rec := saveReapRecord(t, deadPID, time.Now())
	lost := resendHolder(contract.Contract{Holders: []string{rec.ID}})
	if lost != "next" || calls != 1 {
		t.Fatalf("lost holder: got %q after %d re-sends, want next after 1", lost, calls)
	}
	rec.Status, rec.FailKind = "error", agent.FailCancelled
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if got := resendHolder(contract.Contract{Holders: []string{rec.ID}}); got != "" || calls != 1 {
		t.Fatalf("cancelled holder: got %q after %d re-sends, want none", got, calls)
	}
}
