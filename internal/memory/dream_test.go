package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAgentDreamPrompt_UsesReference: a present reference lands in the built prompt's headings.
func TestAgentDreamPrompt_UsesReference(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	refDir := filepath.Join(dir, "skills", "reflection", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "dream.md"), []byte("# Dream procedure\n\nconsolidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := agentDreamPrompt()
	if err != nil {
		t.Fatalf("agentDreamPrompt: %v", err)
	}
	if !strings.Contains(got, "# Dream procedure") {
		t.Errorf("prompt missing the reference heading:\n%s", got)
	}
}

// TestDreamPreCutoverMissingReferenceFailsWithoutDeadlock: on a fresh (pre-cutover) store Dream used to
// re-take its own bank lock inside SettleBank and hang; it must return, report the missing reference,
// and never run the agent.
func TestDreamPreCutoverMissingReferenceFailsWithoutDeadlock(t *testing.T) {
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
		if err == nil || !strings.Contains(err.Error(), "dream") {
			t.Errorf("Dream error = %v, want the missing dream reference", err)
		}
		if ran {
			t.Error("agent ran despite a missing reference")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Dream deadlocked on a pre-cutover store")
	}
}
