package cli

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/mcpx"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
)

// cmdNetworkMCP reads every MCP scope Claude Code uses, runs live connect health, and flags
// cross-scope drift and inline plaintext secrets. Read-only; exit 1 on any Failed server or secret.
func cmdNetworkMCP(args []string) int {
	fs := flag.NewFlagSet("network mcp", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	format := outfmt.Flag(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fail("network mcp: %v", err)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("network mcp: %v", err)
	}
	claude, _ := launch.ClaudeBinary() // "" on error: Doctor fails OPEN on a missing claude binary
	cwd, _ := os.Getwd()
	settingsFiles := []string{
		filepath.Join(paths.ConfigDir(), "settings.json"),
		filepath.Join(paths.ConfigDir(), "settings.local.json"),
	}
	code := mcpx.Doctor(os.Stdout, mcpx.Options{
		UserPath:      paths.ClaudeJSON(),
		ProjectFiles:  []string{filepath.Join(cwd, ".mcp.json")},
		SettingsFiles: settingsFiles,
		Claude:        claude,
		Home:          home,
		JSON:          *format == outfmt.JSON,
	})
	return code
}
