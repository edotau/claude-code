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

const mcpBin = "/custom path/claude-code"

var (
	mcpRemoveArgv = []string{"mcp", "remove", "-s", "user", "web_search"}
	mcpAddArgv    = []string{"mcp", "add", "-s", "user", "web_search", "--", mcpBin, "network", "websearch"}
)

func registerMCPMode(t *testing.T, mode string) (string, error) {
	t.Helper()
	_, log, _ := configureCommandEnv(t, mode)
	if strings.HasPrefix(mode, "exists") {
		doc := `{"mcpServers":{"web_search":{"command":"old-search","args":[]}}}`
		if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return log, configure.RegisterClaudeMCPContext(context.Background(), mcpBin)
}

// TestRegisterClaudeMCPContextFresh: a first registration runs add only, never remove.
func TestRegisterClaudeMCPContextFresh(t *testing.T) {
	log, err := registerMCPMode(t, "normal")
	if err != nil {
		t.Fatal(err)
	}
	assertConfigureArgv(t, log, mcpAddArgv)
}

// TestRegisterClaudeMCPContextReplacesExisting: "already exists" from add triggers remove, then add again.
func TestRegisterClaudeMCPContextReplacesExisting(t *testing.T) {
	log, err := registerMCPMode(t, "exists")
	if err != nil {
		t.Fatal(err)
	}
	assertConfigureArgv(t, log, mcpAddArgv, mcpRemoveArgv, mcpAddArgv)
}

// TestRegisterClaudeMCPContextRemoveFails: a failed remove is reported and add is not retried.
func TestRegisterClaudeMCPContextRemoveFails(t *testing.T) {
	log, err := registerMCPMode(t, "exists-remove-fail")
	if err == nil || !strings.Contains(err.Error(), "claude mcp remove") {
		t.Errorf("remove failure = %v, want claude mcp remove error", err)
	}
	assertConfigureArgv(t, log, mcpAddArgv, mcpRemoveArgv)
}

// TestRegisterClaudeMCPContextAddFails: any other add failure carries the output and never removes.
func TestRegisterClaudeMCPContextAddFails(t *testing.T) {
	log, err := registerMCPMode(t, "add-fail")
	if err == nil || !strings.Contains(err.Error(), "claude mcp add") || !strings.Contains(err.Error(), "command failed") {
		t.Errorf("add failure = %v, want command and output detail", err)
	}
	assertConfigureArgv(t, log, mcpAddArgv)
}

// TestRegisterClaudeMCPContextCancelAdd: cancelling during add returns the context error promptly.
func TestRegisterClaudeMCPContextCancelAdd(t *testing.T) {
	_, log, ready := configureCommandEnv(t, "cancel-add")
	assertConfigureCancellation(t, ready, func(ctx context.Context) error {
		return configure.RegisterClaudeMCPContext(ctx, mcpBin)
	})
	assertConfigureArgv(t, log, mcpAddArgv)
}
