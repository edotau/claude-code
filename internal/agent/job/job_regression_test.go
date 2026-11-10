package job

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

func TestSpawnArgvScrubsInheritedSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HARNESS_KEEP_ENV", "")
	t.Setenv("ANTHROPIC_API_KEY", "fake-inherited-canary")
	t.Setenv("HARNESS_VAULT_KEY", "fake-vault-key")
	script := filepath.Join(dir, "child")
	body := "#!/bin/sh\nprintf '%s\\n' \"$REGRESSION_EXTRA\" \"${ANTHROPIC_API_KEY+set}\" \"$HARNESS_VAULT_KEY\" done\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	old := executable
	executable = func() (string, error) { return script, nil }
	t.Cleanup(func() { executable = old })
	rec := Record{Log: filepath.Join(dir, "child.log")}
	if err := SpawnArgv(&rec, nil, []string{"REGRESSION_EXTRA=explicit-extra"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		got, err := os.ReadFile(rec.Log)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(string(got), "done\n") {
			if string(got) != "explicit-extra\n\nfake-vault-key\ndone\n" {
				t.Fatalf("job child environment = %q; want Extra, secret unset, and Claude vault key retained", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("job child did not finish writing its environment")
		case <-tick.C:
		}
	}
}

func TestCancelDetachedSplitReview(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := executable
	executable = func() (string, error) { return sleeperScript(t), nil }
	t.Cleanup(func() { executable = old })
	rec := ClaimRecord("codex", agent.RunOptions{}, time.Now())
	rec.Progress = "part 1/2"
	if err := SpawnArgv(&rec, []string{"codex", "review", "--background"}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kill(rec.PID) })
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := CancelJob(context.Background(), "codex", "", rec.ID)
	if err != nil {
		t.Fatalf("cancel detached split review: %v", err)
	}
	if got.Alive() || got.Status != "error" || !strings.Contains(got.Error, "cancelled") {
		t.Fatalf("cancelled review = %+v, want stopped and recorded cancellation", got)
	}
	assertSavedCancellation(t, rec.ID)
}
