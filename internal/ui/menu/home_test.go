package menu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/configure"
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

func TestRegisterClaudeMCPContext(t *testing.T) {
	remove := []string{"mcp", "remove", "-s", "user", "web_search"}
	add := []string{"mcp", "add", "-s", "user", "web_search", "--", "/custom path/claude-code", "network", "websearch"}
	for _, mode := range []string{"normal", "remove-fail", "add-fail", "cancel-remove", "cancel-add"} {
		t.Run(mode, func(t *testing.T) {
			_, log, ready := configureCommandEnv(t, mode)
			run := func(ctx context.Context) error {
				return configure.RegisterClaudeMCPContext(ctx, "/custom path/claude-code")
			}
			if strings.HasPrefix(mode, "cancel-") {
				assertConfigureCancellation(t, ready, run)
			} else {
				err := run(context.Background())
				if mode == "add-fail" {
					if err == nil || !strings.Contains(err.Error(), "claude mcp add") ||
						!strings.Contains(err.Error(), "command failed") {
						t.Errorf("add failure = %v, want command and output detail", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancel-remove" {
				assertConfigureArgv(t, log, remove)
			} else {
				assertConfigureArgv(t, log, remove, add)
			}
		})
	}
}
