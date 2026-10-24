package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestAgentDreamPrompt_UsesReference: the embedded dream procedure lands in the built prompt.
func TestAgentDreamPrompt_UsesReference(t *testing.T) {
	got, err := agentDreamPrompt()
	if err != nil {
		t.Fatalf("agentDreamPrompt: %v", err)
	}
	ref, err := Reference("dream")
	if err != nil {
		t.Fatalf("Reference(\"dream\"): %v", err)
	}
	if !strings.Contains(got, ref) {
		t.Errorf("prompt missing the embedded dream reference:\n%s", got)
	}
}

// TestDreamPreCutoverRunsWithoutDeadlock: on a fresh (pre-cutover) store Dream used to re-take its
// own bank lock inside SettleBank and hang. Reference("dream") is now embedded and always resolves
// (the old "missing reference" repro is unreachable), so this instead confirms Dream still returns
// promptly and the agent still runs, proving the lock-then-settle-outside-the-lock fix holds.
func TestDreamPreCutoverRunsWithoutDeadlock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if CutOver() {
		t.Fatal("fixture must be a pre-cutover store")
	}
	ran := false
	run := func(context.Context, string, string) (string, error) { ran = true; return "stub", nil }
	done := make(chan error, 1)
	go func() { done <- Dream(DreamOptions{CWD: t.TempDir(), Run: run}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Dream error = %v, want nil", err)
		}
		if !ran {
			t.Error("agent did not run despite an embedded reference")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Dream deadlocked on a pre-cutover store")
	}
}
