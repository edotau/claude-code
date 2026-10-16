package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/mcpx"
	"github.com/edotau/claude-code/internal/paths"
)

// cmdMCP dispatches the `mcp` verb; doctor is the only subcommand today.
func cmdMCP(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: claude-code mcp doctor [--json]")
		return 2
	}
	switch args[0] {
	case "doctor":
		return cmdMCPDoctor(args[1:])
	}
	return fail("mcp: unknown subcommand %q (usage: claude-code mcp doctor [--json])", args[0])
}

// cmdMCPDoctor reads every MCP scope Claude Code uses, runs live connect health, and flags
// cross-scope drift and inline plaintext secrets. Read-only; exit 1 on any Failed server or secret.
func cmdMCPDoctor(args []string) int {
	fs := flag.NewFlagSet("mcp doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	if fs.Parse(args) != nil {
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
		JSON:          *asJSON,
	})
	return code
}
