package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/tui"
)

// configurePage: --provider pins the backend that serves model calls; --agent scopes to one agent CLI's own setup.
var configurePage = helptext.Page{
	Usage: "claude-code configure [<page>] [flags]\nclaude-code configure --reset|--restore [flags]",
	About: "Guided setup for the model backend, tier pins and each agent CLI; bare walks every page in order. " +
		"--reset re-renders settings.json and the vendor configs from the templates (settings.local.json is " +
		"never touched); with --check it reports drift and exits 1.",
	Commands: [][2]string{
		{"provider", "pick the model backend; --list shows credential state"},
		{"models", "pin tier models (live catalog: claude-code models --live)"},
		{"agents", "set up each agent CLI's routing (--agent for one)"},
		{"router", "start the model router when a slot crosses providers"},
		{"github", "GitHub Copilot logins and opencode's fallback ladder"},
		{"remote", "tailscale connectivity and dispatch hosts (read-only)"},
		{"browser", "browsers Claude in Chrome can drive (read-only)"},
		{"claude", "per-machine settings.json overrides"},
		{"doctor", "read-only health checks"},
	},
}

// keyList is a repeatable comma-separated flag: splits, trims, skips empties, dedupes in first-seen order.
type keyList []string

func (k *keyList) String() string { return strings.Join(*k, ",") }

func (k *keyList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(*k, p) {
			*k = append(*k, p)
		}
	}
	return nil
}

// cmdConfigure is the guided setup: every section in order, or the one named.
func cmdConfigure(args []string) int {
	fs := helptext.NewFlagSet("configure", configurePage)
	provider := fs.String("provider", "", "select the model backend non-interactively")
	vendor := fs.String("agent", "", "set up one CLI: claude|codex|copilot|opencode|gemini")
	dryRun := fs.Bool("dry-run", false, "print what would change; write nothing")
	check := fs.Bool("check", false, "report only; exit 1 if anything is unset")
	reset := fs.Bool("reset", false, "re-render settings.json and the vendor configs from the templates")
	restore := fs.Bool("restore", false, "put vendor configs back from their timestamped backups")
	out := fs.String("out", "", "with --reset: write the render here (after a timestamped backup)")
	vendors := fs.Bool("vendors", false, "with --reset: render vendor configs even off the default ~/.claude")
	list := fs.Bool("list", false, "with provider: list providers and credential state")
	format := outfmt.Flag(fs)
	var drop, keep keyList
	fs.Var(&drop, "drop", "remove these top-level settings.json keys each render (comma or repeat)")
	fs.Var(&keep, "keep", "stop dropping these keys")
	// `configure agents --agent codex`: a leading section word precedes the flags, so lift it out first.
	section := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		section, args = args[0], args[1:]
	}
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if section == "" && len(pos) > 0 {
		section, pos = pos[0], pos[1:]
	}
	if *list || *format != outfmt.Text {
		if section != "provider" {
			return fail("configure: --list needs the provider page")
		}
		name := ""
		if len(pos) > 0 {
			name = pos[0]
		}
		return configureProviderList(name, *format)
	}
	if *reset || *restore {
		return configureRender(*reset, *restore, section, *vendor, *out, *check, *dryRun, *vendors)
	}
	if *out != "" || *vendors {
		return fail("configure: --out and --vendors need --reset")
	}
	return configure.Run(configure.Options{
		Provider: *provider, Vendor: *vendor, DryRun: *dryRun, Check: *check, Drop: drop, Keep: keep, Interactive: isTTY(os.Stdin) && isTTY(os.Stderr),
	}, section)
}

// configureRender runs --reset / --restore; the agent flag is a comma list there.
func configureRender(reset, restore bool, section, agents, out string, check, dryRun, vendors bool) int {
	switch {
	case reset && restore:
		return fail("configure: --reset and --restore are mutually exclusive")
	case section != "":
		return fail("configure: --reset/--restore take no page (got %q)", section)
	case restore && (check || out != "" || vendors):
		return fail("configure: --restore takes only --agent and --dry-run")
	}
	// the engine skips unknown names silently, so a typo would render nothing; reject it here
	for _, a := range strings.Split(agents, ",") {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(generate.DefaultAgents, a) {
			return fail("configure: unknown --agent %q (%s)", a, strings.Join(generate.DefaultAgents, "|"))
		}
	}
	if restore {
		return cmdRestore(agents, dryRun)
	}
	if check && dryRun {
		return fail("configure: --check and --dry-run are mutually exclusive")
	}
	return cmdReset(agents, out, check, dryRun, vendors)
}

// isTTY reports whether f is a terminal (the interactive gate for prompts and login offers).
func isTTY(f *os.File) bool { return ansi.IsTerminal(f) }

func init() {
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
