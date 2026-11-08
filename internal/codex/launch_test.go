package codex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

func TestCodexStatusScrubsVaultKey(t *testing.T) {
	t.Setenv("HARNESS_VAULT_KEY", "probe-secret")
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"key=$HARNESS_VAULT_KEY\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := codexStatus(context.Background(), bin)
	if err != nil || strings.Contains(string(out), "probe-secret") {
		t.Fatalf("codex login status saw the vault key (err=%v): %q", err, out)
	}
}

func TestCodexHeadlessNetworkOnlyWithTools(t *testing.T) {
	const net = "sandbox_workspace_write.network_access=true"
	if got := codexHeadless(nil, &agent.Headless{Tools: true}); !slices.Contains(got, net) {
		t.Fatalf("tools run lacks %s (tests need loopback listeners): %q", net, got)
	}
	if got := codexHeadless(nil, &agent.Headless{}); slices.Contains(got, net) {
		t.Fatalf("read-only run must stay without network: %q", got)
	}
}
