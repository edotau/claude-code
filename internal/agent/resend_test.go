package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/proc"
)

func stubResend(t *testing.T, f func(*exec.Cmd) ([]byte, error)) {
	t.Helper()
	old := resendOutput
	resendOutput = f
	t.Cleanup(func() { resendOutput = old })
}

func saveRec(t *testing.T, r Record) {
	t.Helper()
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestResendReturnsNewID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	saveRec(t, Record{ID: "h-1", Parent: "p-1", Status: "failed", Time: time.Now()})
	var args, env []string
	stubResend(t, func(c *exec.Cmd) ([]byte, error) {
		args, env = c.Args, c.Env
		return []byte(`{"id":"h-2"}`), nil
	})
	got, err := Resend(context.Background(), "h-1")
	if err != nil || got != "h-2" {
		t.Fatalf("Resend = %q, %v; want h-2", got, err)
	}
	if !slices.Contains(args, "--retry") || !slices.Contains(args, "h-1") || !slices.Contains(args, "--recover") {
		t.Fatalf("args = %v", args)
	}
	if !slices.Contains(env, proc.DispatchIDEnv+"=p-1") {
		t.Fatalf("env lacks %s=p-1", proc.DispatchIDEnv)
	}
}

func TestResendIgnoresDeadResumedMarker(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	saveRec(t, Record{ID: "h-1", Parent: "p-1", Status: "failed", Time: time.Now()})
	stubResend(t, func(*exec.Cmd) ([]byte, error) { return nil, errors.New("boom") })
	if err := os.MkdirAll(WatchDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(WatchMarker("resumed", "h-1"), []byte("h-3"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveRec(t, Record{ID: "h-3", Status: "failed", Time: time.Now()})
	if got, err := Resend(context.Background(), "h-1"); err == nil {
		t.Fatalf("Resend = %q, nil; want an error for a dead marker target", got)
	}
	saveRec(t, Record{ID: "h-3", Status: wire.StatusOK, Time: time.Now()})
	if got, err := Resend(context.Background(), "h-1"); err != nil || got != "h-3" {
		t.Fatalf("Resend = %q, %v; want h-3", got, err)
	}
}
