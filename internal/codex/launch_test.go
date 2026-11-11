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

func TestCodexFileOverridesQuotedClosingBracket(t *testing.T) {
	for _, table := range []string{`model_providers."prod]blue"`, `model_providers.'prod]blue'`} {
		t.Run(table, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "[" + table + "] # provider\nname = \"blue\"\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := codexFileOverrides(path)
			if err != nil {
				t.Fatalf("valid quoted table header rejected: %v", err)
			}
			want := []string{"-c", table + `.name="blue"`}
			if !slices.Equal(got, want) {
				t.Fatalf("quoted table override: got %q, want %q", got, want)
			}
		})
	}
}

func TestCodexFileOverridesLongLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	value := `"` + strings.Repeat("x", 80<<10) + `"`
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[model_provider]\ninstructions = "+value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexFileOverrides(path)
	if err != nil {
		t.Fatalf("valid config line over 64 KiB: %v", err)
	}
	want := []string{"-c", "model_provider.instructions=" + value}
	if !slices.Equal(got, want) {
		t.Fatalf("long config value did not round-trip as a table-qualified -c override (got %d args)", len(got))
	}
}

// A tools-off exec drops the skill list, plugins and app connectors; a tools run keeps every one of them.
func TestCodexHeadlessToolsOffTrimsPrompt(t *testing.T) {
	off, on := codexHeadless(nil, &agent.Headless{}), codexHeadless(nil, &agent.Headless{Tools: true})
	for _, c := range []string{"skills.include_instructions=false", "features.plugins=false", "features.apps=false"} {
		if !slices.Contains(off, c) || slices.Contains(on, c) {
			t.Errorf("%s: tools-off has it = %v, tools run has it = %v; want true, false", c, slices.Contains(off, c), slices.Contains(on, c))
		}
	}
	if !slices.Contains(off, "read-only") {
		t.Errorf("tools-off exec lost its read-only sandbox: %q", off)
	}
}
