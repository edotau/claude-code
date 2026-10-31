package configure

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWebSearchClaudeWiredHonoursConfigDir: the claude client reads CLAUDE_CONFIG_DIR/.claude.json, not $HOME/.claude.json.
func TestWebSearchClaudeWiredHonoursConfigDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	doc := `{"mcpServers":{"web_search":{"command":"/x/claude-code","args":["network","websearch"]}}}`
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if !webSearchClients()[0].wired() {
		t.Fatal("claude client must read the wired .claude.json under CLAUDE_CONFIG_DIR")
	}
}
