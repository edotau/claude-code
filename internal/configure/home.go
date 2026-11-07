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
	"github.com/edotau/claude-code/internal/agentconfig"
	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/codex"
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

// codexMCPHeader recognizes the web_search table, including quoted keys and trailing comments.
var codexMCPHeader = regexp.MustCompile(`^\[\s*(?:mcp_servers|"mcp_servers"|'mcp_servers')\s*\.\s*(?:web_search|"web_search"|'web_search')\s*\]\s*(?:#.*)?$`)

// codexMCPRange locates the real table without treating multiline strings or arrays as headers.
func codexMCPRange(raw []byte) (start, end int) {
	start, offset := -1, 0
	multiline, depth := "", 0
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if multiline == "" && depth == 0 && strings.HasPrefix(trimmed, "[") {
			if start >= 0 {
				return start, offset
			}
			if codexMCPHeader.MatchString(trimmed) {
				start = offset
			}
		}
		multiline, depth = agentconfig.TOMLLineState(line, multiline, depth)
		offset += len(line)
	}
	return start, len(raw)
}

func CodexWired(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	start, end := codexMCPRange(raw)
	return start >= 0 && strings.Contains(string(raw[start:end]), "claude-code") && strings.Contains(string(raw[start:end]), `args = ["network", "websearch"]`)
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
	out, err := mcpAddWebSearch(ctx, claude, bin)
	if err != nil && strings.Contains(string(out), "already exists") {
		// Replace only after add proved the CLI works and the entry exists; remove errors are reported.
		if rerr := exec.CommandContext(ctx, claude, "mcp", "remove", "-s", "user", "web_search").Run(); rerr != nil {
			return fmt.Errorf("claude mcp remove: %w", rerr)
		}
		out, err = mcpAddWebSearch(ctx, claude, bin)
	}
	if cerr := ctxErr(ctx, "claude mcp add"); cerr != nil {
		return cerr
	}
	if err != nil {
		return fmt.Errorf("claude mcp add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func mcpAddWebSearch(ctx context.Context, claude, bin string) ([]byte, error) {
	return exec.CommandContext(ctx, claude, "mcp", "add", "-s", "user", "web_search", "--", bin, "network", "websearch").CombinedOutput()
}

// codexMCPExtras pre-approves web_search (approval "never") and passes the keyring address so it opens the vault.
const codexMCPExtras = "default_tools_approval_mode = \"approve\"\nenv_vars = [\"DBUS_SESSION_BUS_ADDRESS\", \"XDG_RUNTIME_DIR\"]\n"

// RegisterCodexMCP replaces (or appends) the web_search table, sealing the previous file in the vault backups.
func RegisterCodexMCP(path, bin string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	block := fmt.Sprintf("[mcp_servers.web_search]\ncommand = %q\nargs = [\"network\", \"websearch\"]\n%s\n", bin, codexMCPExtras)
	start, end := codexMCPRange(raw)
	var next []byte
	if start < 0 {
		next = []byte(agentconfig.AppendTOML(string(raw), block))
	} else {
		next = []byte(string(raw[:start]) + block + string(raw[end:]))
	}
	if len(raw) > 0 {
		if err := codex.CodexBackup(path, raw, true); err != nil {
			return err
		}
	}
	return paths.AtomicWrite(path, next, 0o600)
}

// RenderAgentConfigs re-renders the agent configs, writing progress to out.
func RenderAgentConfigs(out io.Writer) error {
	_, err := agent.RenderConfigs(agentrender.RenderOptions{Out: out})
	return err
}
