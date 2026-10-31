package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureRecord saves rec under a fresh CLAUDE_CONFIG_DIR scoped to t, returning the saved record.
func fixtureRecord(t *testing.T, rec Record) Record {
	t.Helper()
	if err := rec.Save(); err != nil {
		t.Fatalf("Save(%+v): %v", rec, err)
	}
	return rec
}

// adapterDeadPID starts and waits out a child process, so its pid is guaranteed not to be alive.
func adapterDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn+wait a short-lived process: %v", err)
	}
	return cmd.Process.Pid
}

func setConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// TestStatusUnknownVendorErrors: Status rejects an unregistered vendor name before touching the dispatch log.
func TestStatusUnknownVendorErrors(t *testing.T) {
	setConfigDir(t)
	if _, err := Status(context.Background(), "nonexistent-vendor", t.TempDir(), "", false); err == nil {
		t.Error("Status(unknown agent) = nil error, want errUnknownAgent")
	}
}

// TestStatusRunningAlive: a running record whose pid is this test process reports Status "running".
func TestStatusRunningAlive(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "do the thing"}, base)
	rec.Repo, rec.PID = cwd, os.Getpid()
	fixtureRecord(t, rec)

	rows, err := Status(context.Background(), "codex", cwd, "", false)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Status rows = %v, want 1", rows)
	}
	if rows[0].Status != "running" {
		t.Errorf("rows[0].Status = %q, want running (pid %d is this test process)", rows[0].Status, os.Getpid())
	}
	if rows[0].Preview != "do the thing" {
		t.Errorf("rows[0].Preview = %q, want %q", rows[0].Preview, "do the thing")
	}
}

// TestStatusRunningLostDeadPID: a record still marked running but whose pid has exited reports "lost",
// not "running" — this is the teeth case: Alive(rec) must actually gate the row, not just echo r.Status.
func TestStatusRunningLostDeadPID(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "orphaned"}, base)
	rec.Repo, rec.PID = cwd, adapterDeadPID(t)
	fixtureRecord(t, rec)

	rows, err := Status(context.Background(), "codex", cwd, "", false)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "lost" {
		t.Fatalf("Status rows = %v, want one row with Status lost", rows)
	}
}

// TestResultRunningErrors: a still-running record, fetched by its exact id, is refused with "still running" —
// JobResult must not hand back a log for a job that has not finished.
func TestResultRunningErrors(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "still going"}, base)
	rec.Repo, rec.PID = cwd, os.Getpid()
	fixtureRecord(t, rec)

	_, _, err := JobResult(context.Background(), "codex", cwd, rec.ID)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("Result(running job) err = %v, want it to mention \"still running\"", err)
	}
}

// TestResultOK: a finished record returns its record and log text.
func TestResultOK(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	logPath := filepath.Join(cwd, "job.log")
	if err := os.WriteFile(logPath, []byte("the answer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "finished job"}, base)
	rec.Repo, rec.Status, rec.Log, rec.SessionID = cwd, "ok", logPath, "sess-1"
	fixtureRecord(t, rec)

	got, log, err := JobResult(context.Background(), "codex", cwd, rec.ID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got.ID != rec.ID || got.SessionID != "sess-1" {
		t.Errorf("Result record = %+v, want ID %q SessionID sess-1", got, rec.ID)
	}
	if log != "the answer\n" {
		t.Errorf("Result log = %q, want %q", log, "the answer\n")
	}
}

// TestCancelJobNoneRunningErrors: with no running job for this vendor/repo, CancelJob errors rather
// than cancelling an unrelated finished record.
func TestCancelJobNoneRunningErrors(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "already done"}, base)
	rec.Repo, rec.Status = cwd, "ok"
	fixtureRecord(t, rec)

	if _, err := CancelJob(context.Background(), "codex", cwd, ""); err == nil {
		t.Error("CancelJob with zero running jobs = nil error, want one")
	}
}

// TestCancelJobAmbiguousPrefixErrors: a ref prefix matching several dispatches is refused rather than
// guessing which one to cancel.
func TestCancelJobAmbiguousPrefixErrors(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, suffix := range []string{"-a", "-b"} {
		rec := NewRecord("codex", RunOptions{Prompt: "dup"}, base)
		rec.ID = "20261024T120000.000-codex" + suffix
		rec.Repo, rec.PID = cwd, os.Getpid()
		fixtureRecord(t, rec)
	}

	_, err := CancelJob(context.Background(), "codex", cwd, "20261024T120000")
	if err == nil || !strings.Contains(err.Error(), "matches 2 dispatches") {
		t.Errorf("CancelJob(ambiguous prefix) err = %v, want \"matches 2 dispatches\"", err)
	}
}

// TestCancelJobUnknownVendorErrors: CancelJob rejects an unregistered vendor before touching the log.
func TestCancelJobUnknownVendorErrors(t *testing.T) {
	setConfigDir(t)
	if _, err := CancelJob(context.Background(), "nonexistent-vendor", t.TempDir(), ""); err == nil {
		t.Error("CancelJob(unknown agent) = nil error, want errUnknownAgent")
	}
}
