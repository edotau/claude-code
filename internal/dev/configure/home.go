// home.go — the configure home page's tools and settings sections, the settings re-render on exit, and restore/revert.
package configure

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/cli/ui/tui"
	"github.com/edotau/claude-code/internal/driver/codex"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
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

func CodexWired(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	start, end := codex.MCPTableRange(raw, "web_search")
	if start < 0 {
		return false
	}
	command, argsOK := codexMCPEntry(string(raw[start:end]))
	return filepath.Base(command) == "claude-code" && argsOK
}

// codexMCPEntry reads the command value and whether args is exactly codexArgs from one MCP table's text.
func codexMCPEntry(table string) (command string, argsOK bool) {
	state, depth := "", 0
	for line := range strings.SplitSeq(table, "\n") {
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
	return command, argsOK
}

// codexArgs is the web_search table's args value.
var codexArgs = `["` + strings.Join(webSearchArgs, `", "`) + `"]`

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
	claude, err := paths.NativeClaude()
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
	argv := append([]string{"mcp", "add", "-s", "user", "web_search", "--", bin}, webSearchArgs...)
	return exec.CommandContext(ctx, claude, argv...).CombinedOutput()
}

// RegisterCodexMCP replaces (or appends) the web_search table, sealing the previous file in the vault backups.
func RegisterCodexMCP(path, bin string) error {
	return codex.WithConfigLock(path, func() error { return registerCodexMCPLocked(path, bin) })
}

func registerCodexMCPLocked(path, bin string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	block := codex.MCPTable("web_search", bin, webSearchArgs) + "\n"
	start, end := codex.MCPTableRange(raw, "web_search")
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

// Restore resets settings.json (backup first), then renders agent configs; --out skips the render, --agents the reset.
func Restore(w io.Writer, agents, out string, forceHomes bool) error {
	if agents != "" || forceHomes {
		return confirmAndRender(w, agents, forceHomes)
	}
	target := cmp.Or(out, settings.SettingsFile())
	backup, err := settings.ResetSettings(target)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "restored %s", target)
	if backup != "" {
		fmt.Fprintf(w, " (backup %s)", backup)
	}
	fmt.Fprintln(w)
	if out != "" {
		return nil // "this file instead": the live agent configs stay untouched
	}
	return confirmAndRender(w, "", false)
}

// confirmAndRender renders the agent configs, asking before an overwrite on a real terminal.
func confirmAndRender(w io.Writer, agents string, forceHomes bool) error {
	opts := agentconfig.RenderOptions{Agents: textutil.SplitCSV(agents), Out: w, ForceHomes: forceHomes}
	if tui.Interactive() {
		opts.Confirm = func(path string, lost []string) bool {
			return tui.ConfirmKey(fmt.Sprintf("overwrite %s?", path), true)
		}
	}
	prepared, _, err := agent.PrepareConfigs(opts)
	if err != nil || prepared == nil {
		return err
	}
	return prepared.Apply()
}

// Revert is `configure revert [--agent a,b] [--dry-run]`.
func Revert(w io.Writer, agents string, dryRun bool) error {
	return agent.RevertConfigs(agentconfig.RenderOptions{Agents: textutil.SplitCSV(agents), DryRun: dryRun, Out: w})
}
