package copilot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCopilotTierCmdScrubsInheritedSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", dir)
	t.Setenv("HARNESS_KEEP_ENV", "")
	t.Setenv("ANTHROPIC_API_KEY", "fake-inherited-canary")
	t.Setenv("GH_TOKEN", "fake-inherited-github-token")
	script := "#!/bin/sh\nprintf '%s\\n' \"$GH_TOKEN\" \"${ANTHROPIC_API_KEY+set}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := copilotTierCmd(ctx, "fake-explicit-github-token")
	if err != nil {
		t.Fatalf("real gh child: %v", err)
	}
	if string(got) != "fake-explicit-github-token\n\n" {
		t.Fatalf("gh child environment = %q; want explicit GH_TOKEN and ANTHROPIC_API_KEY unset", got)
	}
}
