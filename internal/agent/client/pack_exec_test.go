package client

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// Given a pack file that cannot be written, Then Exec fails and the claimed record leaves running.
func TestExecPackWriteFailureFinishesRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := job.ClaimRecord("claude", agent.RunOptions{}, time.Now())
	p := &Prepared{Record: rec, pack: &packFile{path: filepath.Join(t.TempDir(), "missing", "pack.md"), body: "x"}}
	res, err := (&Client{}).Exec(context.Background(), p, wire.Discard, job.RunRecordedOpts{})
	if err == nil || res.Status != wire.StatusError {
		t.Fatalf("Exec = %v/%v, want the write error", res.Status, err)
	}
	got, lerr := job.LoadRecord(rec.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got.Status != "error" {
		t.Errorf("record status = %q, want error", got.Status)
	}
}
