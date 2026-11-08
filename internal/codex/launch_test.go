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

// Tool runs stay sealed by default: network only with HARNESS_CODEX_NETWORK=1, and never for read-only runs.
func TestCodexHeadlessNetworkOptIn(t *testing.T) {
	const net = "sandbox_workspace_write.network_access=true"
	t.Setenv(codexNetworkEnv, "")
	if got := codexHeadless(nil, &agent.Headless{Tools: true}); slices.Contains(got, net) {
		t.Fatalf("default tools run must stay sealed: %q", got)
	}
	t.Setenv(codexNetworkEnv, "1")
	if got := codexHeadless(nil, &agent.Headless{Tools: true}); !slices.Contains(got, net) {
		t.Fatalf("opted-in tools run lacks %s: %q", net, got)
	}
	if got := codexHeadless(nil, &agent.Headless{}); slices.Contains(got, net) {
		t.Fatalf("read-only run must stay sealed even when opted in: %q", got)
	}
}
