package job

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

type noLauncher struct{ agent.Launcher }

func TestRunHolderRestoresLauncher(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var got *Record
	orig := serveLive
	serveLive = func(_ context.Context, rec *Record) error { got = rec; return nil }
	t.Cleanup(func() { serveLive = orig })

	rec := &Record{ID: "live-holder-test"}
	if code := RunHolder(rec, noLauncher{}); code != 0 {
		t.Fatalf("RunHolder = %d, want 0", code)
	}
	if got == nil || got.Request.Launcher == nil {
		t.Fatal("ServeLive got a record with no Launcher (json:\"-\" field lost on reload)")
	}
}

// Given a holder that fails with a credential-shaped error, Then the saved record is redacted and carries a duration.
func TestRunHolderRedactsAndTimesFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const secret = "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	orig := serveLive
	serveLive = func(context.Context, *Record) error { return fmt.Errorf("claude exited: token %s", secret) }
	t.Cleanup(func() { serveLive = orig })

	rec := ClaimRecord("claude", agent.RunOptions{}, time.Now().Add(-2*time.Second))
	if code := RunHolder(&rec, noLauncher{}); code != 1 {
		t.Fatalf("RunHolder = %d, want 1", code)
	}
	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || strings.Contains(got.Error, secret) {
		t.Errorf("record = %q/%q, want error status with the secret redacted", got.Status, got.Error)
	}
	if got.DurationMs < 1000 {
		t.Errorf("DurationMs = %d, want the failed holder's elapsed time", got.DurationMs)
	}
}
