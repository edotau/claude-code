package usage

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClaudePressureCancelsStalledCredentials(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("credential lookup does not use the keychain on this platform")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	safety, stop := context.WithTimeout(context.Background(), 4*time.Second)
	defer stop()
	var lookupErr error
	stubClaudeKeychain(t, func(ctx context.Context, _ ...string) ([]byte, error) {
		select {
		case <-ctx.Done():
			lookupErr = ctx.Err()
		case <-safety.Done():
			return nil, safety.Err()
		}
		return nil, lookupErr
	})
	start := time.Now()
	pct, window, ok := ClaudePressure(start)
	if !errors.Is(lookupErr, context.DeadlineExceeded) {
		t.Fatalf("stalled credential lookup was not cancelled by ClaudePressure: %v", lookupErr)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ClaudePressure exceeded its 2s credential budget: %s", elapsed)
	}
	if pct != 0 || window != "" || ok {
		t.Fatalf("cancelled credential lookup returned pressure: pct=%v window=%q ok=%v", pct, window, ok)
	}
}

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
