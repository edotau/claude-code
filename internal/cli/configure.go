package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/tui"
)

// configureUsage: --provider pins the backend that serves model calls; --agent scopes to one agent CLI's own setup.
var configureUsage = "usage: claude-code configure [--provider p] [--agent CLI] [--dry-run] [--check]\n       [--drop k[,k…]] [--keep k[,k…]]\n       [" +
	strings.Join(configure.Sections, "|") + "]\n" + `       claude-code configure --reset [--check|--dry-run] [--agent a,b] [--out f]
                                     [--vendors]
       claude-code configure --restore [--agent a,b] [--dry-run]

--reset re-renders settings.json and the vendor configs from the templates;
settings.local.json is never touched. --check reports drift, exit 1.
--restore puts vendor configs back from their timestamped backups.`

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
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	provider := fs.String("provider", "", "select the model backend non-interactively")
	vendor := fs.String("agent", "", "set up one CLI: claude|codex|copilot|opencode|gemini")
	dryRun := fs.Bool("dry-run", false, "print what would change; write nothing")
	check := fs.Bool("check", false, "report only; exit 1 if anything is unset")
	reset := fs.Bool("reset", false, "re-render settings.json and the vendor configs from the templates")
	restore := fs.Bool("restore", false, "put vendor configs back from their timestamped backups")
	out := fs.String("out", "", "with --reset: write the render here (after a timestamped backup)")
	vendors := fs.Bool("vendors", false, "with --reset: render vendor configs even off the default ~/.claude")
	var drop, keep keyList
	fs.Var(&drop, "drop", "remove these top-level settings.json keys each render (comma or repeat)")
	fs.Var(&keep, "keep", "stop dropping these keys")
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), configureUsage+"\n")
		helptext.Defaults(fs)
	}
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
