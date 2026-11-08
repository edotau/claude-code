// Package rendertest is the shared hermetic fixture for vendor render tests.
package rendertest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Scratch points CLAUDE_CONFIG_DIR at a temp dir with a keyed default provider and returns the vendor-config home.
func Scratch(t *testing.T) string {
	t.Helper()
	configDir := vaulttest.Use(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "")
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Cleanup(agent.StubRouter("http://127.0.0.1:9999"))
	vaulttest.Put(t, vault.Overlay, "providers", `{"default": "openai"}`)
	if err := os.MkdirAll(filepath.Join(configDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}
