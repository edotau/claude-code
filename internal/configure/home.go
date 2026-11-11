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
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
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
	if start < 0 {
		return false
	}
	var command string
	var argsOK bool
	state, depth := "", 0
	for line := range strings.SplitSeq(string(raw[start:end]), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && state == "" && depth == 0 { // text inside strings is no key
			value = strings.TrimSpace(value)
			switch strings.TrimSpace(key) {
			case "command":
				command, _ = agentconfig.TOMLStringValue(value)
			case "args":
				rest, found := strings.CutPrefix(value, codexArgs)
				rest = strings.TrimSpace(rest)
				argsOK = found && (rest == "" || strings.HasPrefix(rest, "#"))
			}
		}
		state, depth = agentconfig.TOMLLineState(line, state, depth)
	}
	return filepath.Base(command) == "claude-code" && argsOK
}

// codexArgs is the web_search table's args value.
const codexArgs = `["network", "websearch"]`

func RegisterClaudeMCP(bin string) error {
	return withInterrupt(func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return registerClaudeMCPContext(ctx, bin)
	})
}

func registerClaudeMCPContext(ctx context.Context, bin string) error {
	if err := ctxErr(ctx, "claude mcp"); err != nil {
		return err
	}
	claude, err := paths.AgentBin("claude")
	if err != nil {
		return err
	}
	out, err := mcpAddWebSearch(ctx, claude, bin)
	if err != nil && strings.Contains(string(out), "already exists") {
		return replaceClaudeMCP(ctx, claude, bin)
	}
	return claudeAddErr(ctx, out, err)
}

// replaceClaudeMCP swaps the existing web_search entry for bin; a failed re-add restores the saved entry.
func replaceClaudeMCP(ctx context.Context, claude, bin string) error {
	prev, err := claudeMCPEntry(paths.ClaudeJSON())
	if err != nil {
		return fmt.Errorf("claude mcp: existing web_search not saved, refusing to replace it: %w", err)
	}
	if err := exec.CommandContext(ctx, claude, "mcp", "remove", "-s", "user", "web_search").Run(); err != nil {
		return fmt.Errorf("claude mcp remove: %w", err)
	}
	out, err := mcpAddWebSearch(ctx, claude, bin)
	if err == nil {
		return nil
	}
	if rerr := restoreClaudeMCP(ctx, claude, prev); rerr != nil {
		return errors.Join(claudeAddErr(ctx, out, err), fmt.Errorf("restore web_search: %w", rerr))
	}
	return claudeAddErr(ctx, out, err)
}

// claudeMCPEntry returns the raw user-scope web_search entry from the Claude config file at path.
func claudeMCPEntry(path string) (json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	entry, ok := cfg.MCPServers["web_search"]
	if !ok {
		return nil, fmt.Errorf("no user-scope web_search entry in %s", path)
	}
	return entry, nil
}

// restoreClaudeMCP re-adds the saved entry with its original JSON; it runs even when ctx has expired.
func restoreClaudeMCP(ctx context.Context, claude string, entry json.RawMessage) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(rctx, claude, "mcp", "add-json", "-s", "user", "web_search", string(entry)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// claudeAddErr maps one `claude mcp add` result to an error: context first, then the CLI's own failure.
func claudeAddErr(ctx context.Context, out []byte, err error) error {
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
	_, err := agent.RenderConfigs(agentconfig.RenderOptions{Out: out})
	return err
}
