package usage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cancelAfterChecksContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelAfterChecksContext) Err() error {
	c.checks--
	if c.checks == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCodexUsageCancelsMidTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	fixture := string(readFixture(t, "codex-session-fixture.jsonl"))
	const repetitions = 8192
	writeFile(t, filepath.Join(home, "sessions", "rollout.jsonl"), strings.Repeat(fixture, repetitions))
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Six checks allow traversal and two 1MiB parser reads before cancelling inside the only transcript.
	ctx := &cancelAfterChecksContext{Context: base, cancel: cancel, checks: 6}
	row := codexUsage(ctx, time.Time{})
	fullTurns := strings.Count(fixture, `"type":"token_count"`) * repetitions
	if row.Sessions != 1 || row.Turns <= 0 || row.Turns >= fullTurns {
		t.Fatalf("expected a partially read final transcript, got %+v", row)
	}
	if !errors.Is(base.Err(), context.Canceled) {
		t.Fatalf("context did not cancel during transcript parsing: %v", base.Err())
	}
	if !strings.Contains(row.Note, "incomplete:") || !strings.Contains(row.Note, context.Canceled.Error()) {
		t.Fatalf("mid-transcript cancellation produced complete-looking totals: %+v", row)
	}
}
