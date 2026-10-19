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

// cmdMCP dispatches the `mcp` verb; doctor is the only subcommand today.
func cmdMCP(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		helptext.Fprint(os.Stderr, "usage: claude-code mcp doctor [--format text|json]\n")
		return 2
	}
	switch args[0] {
	case "doctor":
		return cmdMCPDoctor(args[1:])
	}
	fail("mcp: unknown subcommand %q", args[0])
	helptext.Fprint(os.Stderr, "usage: claude-code mcp doctor [--format text|json]\n")
	return 1
}

// cmdMCPDoctor reads every MCP scope Claude Code uses, runs live connect health, and flags
// cross-scope drift and inline plaintext secrets. Read-only; exit 1 on any Failed server or secret.
func cmdMCPDoctor(args []string) int {
	fs := flag.NewFlagSet("mcp doctor", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	format := outfmt.Flag(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fail("mcp doctor: %v", err)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("mcp doctor: %v", err)
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
