package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

// waitRecord saves a dispatch record with the given id and status, owned by this live process.
func waitRecord(t *testing.T, id, status string) Record {
	t.Helper()
	rec := NewRecord("claude", RunOptions{Prompt: "p"}, time.Now())
	rec.ID, rec.Status, rec.PID, rec.DurationMs = id, status, os.Getpid(), 1500
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

// Wait returns rows in id order, blocks on a running dispatch until it settles, and reports an error as its own state.
func TestWaitSettlesEveryDispatchInOrder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	waitRecord(t, "w-ok", "ok")
	waitRecord(t, "w-err", "error")
	late := waitRecord(t, "w-late", "running")
	go func() {
		time.Sleep(100 * time.Millisecond)
		late.Status = "ok"
		_ = late.Save()
	}()
	got, err := Wait(context.Background(), []string{"w-err", "w-late", "w-ok"}, 5*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"w-err error 1.5s", "w-late done 1.5s", "w-ok done 1.5s"}
	for i, w := range want {
		if got[i].Line() != w {
			t.Errorf("row %d = %q, want %q", i, got[i].Line(), w)
		}
	}
}

// A dispatch still running at the deadline is marked timeout; an unknown id fails before any wait.
func TestWaitTimeoutAndUnknownID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	waitRecord(t, "w-run", "running")
	got, err := Wait(context.Background(), []string{"w-run"}, 50*time.Millisecond, 10*time.Millisecond)
	if err != nil || got[0].State != WaitTimeout {
		t.Fatalf("Wait = %+v, %v; want state timeout", got, err)
	}
	if _, err := Wait(context.Background(), []string{"w-none"}, time.Second, time.Millisecond); err == nil {
		t.Error("unknown id: want an error")
	}
}
