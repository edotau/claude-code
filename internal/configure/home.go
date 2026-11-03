// home.go — the configure home page's tools and settings sections, and the settings re-render on exit.
package configure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/paths"
)

// webSearchArgs is the claude-code argv every client registers for the web_search MCP server.
var webSearchArgs = []string{"network", "websearch"}

// ClaudeJSONWired: ~/.claude.json's user-scope web_search runs `claude-code network websearch`.
func ClaudeJSONWired(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false
	}
	s, ok := cfg.MCPServers["web_search"]
	return ok && filepath.Base(s.Command) == "claude-code" && slices.Equal(s.Args, webSearchArgs)
}

// codexMCPBlock matches config.toml's [mcp_servers.web_search] table up to the next table.
var codexMCPBlock = regexp.MustCompile(`(?m)^\[mcp_servers\.web_search\]\n(?:[^\[\n].*\n|\n)*`)

func CodexWired(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	b := codexMCPBlock.Find(raw)
	return b != nil && strings.Contains(string(b), "claude-code") && strings.Contains(string(b), `args = ["network", "websearch"]`)
}

func RegisterClaudeMCP(bin string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return RegisterClaudeMCPContext(ctx, bin)
}

func RegisterClaudeMCPContext(ctx context.Context, bin string) error {
	if err := ctxErr(ctx, "claude mcp"); err != nil {
		return err
	}
	claude, err := paths.AgentBin("claude")
	if err != nil {
		return err
	}
	_ = exec.CommandContext(ctx, claude, "mcp", "remove", "-s", "user", "web_search").Run()
	if err := ctxErr(ctx, "claude mcp remove"); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, claude, "mcp", "add", "-s", "user", "web_search", "--", bin, "network", "websearch").CombinedOutput()
	if cerr := ctxErr(ctx, "claude mcp add"); cerr != nil {
		return cerr
	}
	if err != nil {
		return fmt.Errorf("claude mcp add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RegisterCodexMCP replaces (or appends) the web_search table, sealing the previous file in the vault backups.
func RegisterCodexMCP(path, bin string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	block := fmt.Sprintf("[mcp_servers.web_search]\ncommand = %q\nargs = [\"network\", \"websearch\"]\n\n", bin)
	next := codexMCPBlock.ReplaceAllLiteral(raw, []byte(block))
	if !codexMCPBlock.Match(raw) {
		next = append(append(raw, '\n'), block...)
	}
	if len(raw) > 0 {
		if err := codex.CodexBackup(path, raw, true); err != nil {
			return err
		}
	}
	return paths.AtomicWrite(path, next, 0o600)
}

// RerenderSettings re-renders settings.json (merge: foreign keys/hooks kept, backup first) and returns the
// line to print; an error means settings.json could not be brought current.
func RerenderSettings() (string, error) {
	_, existing, merged, _, backup, err := launch.ApplySettings(false)
	switch {
	case err != nil:
		return "", err
	case backup == "" && string(merged) == string(existing):
		return "settings.json unchanged", nil
	default:
		return fmt.Sprintf("settings.json updated (backup %s)", backup), nil
	}
}

// RenderAgentConfigs re-renders the agent configs, writing progress to out.
func RenderAgentConfigs(out io.Writer) error {
	_, err := agent.RenderConfigs(agentrender.RenderOptions{Out: out})
	return err
}
