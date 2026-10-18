package cli

import (
	"errors"
	"flag"
	"fmt"
	"github.com/edotau/claude-code/internal/ansi"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/tui"
)

// configureUsage: --provider pins the backend that serves model calls; --agent scopes to one agent CLI's own setup.
var configureUsage = "usage: claude-code configure [--provider p] [--agent claude|codex|copilot|opencode|gemini] [--dry-run] [--check] [" +
	strings.Join(configure.Sections, "|") + "]"

// cmdConfigure is the guided setup: every section in order, or the one named.
func cmdConfigure(args []string) int {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	provider := fs.String("provider", "", "backend that serves model calls (subscription, anthropic, chatgpt, gemini, …), picked non-interactively")
	vendor := fs.String("agent", "", "scope keys/agents to one agent CLI: claude|codex|copilot|opencode|gemini")
	dryRun := fs.Bool("dry-run", false, "print what would change; write nothing")
	check := fs.Bool("check", false, "report only; exit 1 if anything is unset")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, configureUsage) }
	// `configure agents --agent codex`: a leading section word precedes the flags, so lift it out first.
	section := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		section, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if section == "" && fs.NArg() > 0 {
		section = fs.Arg(0)
	}
	return configure.Run(configure.Options{
		Provider: *provider, Vendor: *vendor, DryRun: *dryRun, Check: *check, Interactive: isTTY(os.Stdin) && isTTY(os.Stderr),
	}, section)
}

// isTTY reports whether f is a terminal (the interactive gate for prompts and login offers).
func isTTY(f *os.File) bool { return ansi.IsTerminal(f) }

func init() {
	configureRun = cmdConfigure
	// A vendor the harness cannot route gets one interactive setup offer before the plain-CLI passthrough.
	launch.OfferConfigure = func(vendor string, reason error) bool {
		if !isTTY(os.Stdin) || !isTTY(os.Stderr) {
			return false
		}
		fmt.Fprintln(os.Stderr, reason)
		if !tui.Confirm("Set up "+vendor+" now?", true) {
			return false
		}
		changed, err := configure.Vendor(vendor, true, os.Stderr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "configure %s: %v\n", vendor, err)
		}
		return changed
	}
}
