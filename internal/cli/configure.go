package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/desktop"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/menu"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// configurePage: --profile picks the profile that serves model calls; --agent scopes to one agent CLI's own setup.
var configurePage = helptext.Page{
	Usage: "claude-code configure [<page>] [flags]\n" +
		"claude-code configure --restore|--revert [flags]\n" +
		"claude-code configure provider --all [<profile>] [--check]",
	About: "Guided setup for the profile, model pins and each agent CLI; bare walks every page in order. " +
		"--restore rewrites settings.json from your defaults and re-renders the agent configs " +
		"(settings.local.json is never touched); with --check it reports drift and exits 1. " +
		"--revert puts agent configs back from their backups.",
	Commands: [][2]string{
		{"provider", "pick the session profile; --list shows credential state"},
		{"models", "pin models (live catalog: claude-code models --live)"},
		{"agents", "set up each agent CLI's routing (--agent for one)"},
		{"github", "GitHub Copilot logins and opencode's fallback ladder"},
		{"remote", "tailscale connectivity and remote hosts (read-only)"},
		{"claude", "per-machine settings.json overrides"},
		{"desktop", "Claude Desktop loopback profile; --revert restores selection"},
		{"codex", "Codex preferences with encrypted config history"},
		{"routing", "price weight, subagent offload, opencode's sub-pick knobs"},
		{"databricks", "browser sign-in and token mint per Databricks profile"},
	},
}

// keyList is a repeatable comma-separated flag: splits, trims, skips empties, dedupes in first-seen order.
type keyList []string

func (k *keyList) String() string { return strings.Join(*k, ",") }

// Placeholder names the FLAGS placeholder for keyList (helptext.Placeholder).
func (k *keyList) Placeholder() string { return "KEY,KEY" }

func (k *keyList) Set(v string) error {
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(*k, p) {
			*k = append(*k, p)
		}
	}
	return nil
}

// cmdConfigure is the guided setup: every section in order, or the one named.
func cmdConfigure(args []string) int {
	if len(args) > 0 && args[0] == "desktop" {
		return cmdConfigureDesktop(args[1:])
	}
	fs := helptext.NewFlagSet("configure", configurePage)
	provider := fs.String("profile", "", "select the session profile non-interactively")
	agent := fs.String("agent", "", "set up one CLI: claude|codex|copilot|opencode|gemini")
	dryRun := fs.Bool("dry-run", false, "print what would change; write nothing")
	check := fs.Bool("check", false, "report only; exit 1 if anything is unset")
	restore := fs.Bool("restore", false, "rewrite settings.json from your defaults and re-render the agent configs")
	revert := fs.Bool("revert", false, "put agent configs back from their timestamped backups")
	out := fs.String("out", "", "with --restore: write the render here (after a timestamped backup)")
	agents := fs.Bool("agents", false, "with --restore: render agent configs even off the default ~/.claude")
	noAgents := fs.Bool("no-agents", false, "with --restore: settings.json only, skip the agent configs")
	list := fs.Bool("list", false, "with provider: list profiles and credential state")
	all := fs.Bool("all", false, "with provider: check every profile's sign-in and log in where needed")
	format := helptext.FormatFlag(fs)
	var drop, keep keyList
	fs.Var(&drop, "drop", "remove these top-level settings.json keys each render (comma or repeat)")
	fs.Var(&keep, "keep", "stop dropping these keys")
	// `configure agents --agent codex`: a leading section word precedes the flags, so lift it out first.
	section := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		section, args = args[0], args[1:]
	}
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if section == "" && len(pos) > 0 {
		section, pos = pos[0], pos[1:]
	}
	if *all && section != "provider" {
		return fail("configure: --all needs the provider page")
	}
	profile := ""
	if *all && (*list || *provider != "") {
		return fail("configure: --all cannot be combined with --list or --profile")
	}
	if *all && len(pos) > 1 {
		return fail("configure: --all takes at most one <profile> (got %d)", len(pos))
	}
	if *all && len(pos) > 0 {
		profile = pos[0]
		if err := knownProfile(profile); err != nil {
			return fail("configure: %v", err)
		}
	}
	if *list || *format != helptext.Text && !*all {
		if section != "provider" {
			return fail("configure: --list needs the provider page")
		}
		name := ""
		if len(pos) > 0 {
			name = pos[0]
		}
		return configureProviderList(name, *format)
	}
	if *restore || *revert {
		return configureRender(*restore, *revert, section, *agent, *out, *check, *dryRun, *agents, *noAgents)
	}
	if *out != "" || *agents || *noAgents {
		return fail("configure: --out, --agents and --no-agents need --restore")
	}
	return menu.Run(menu.Options{
		Provider: *provider, Agent: *agent, DryRun: *dryRun, Check: *check, All: *all, Profile: profile,
		JSON: *format == helptext.JSON, Drop: drop, Keep: keep, Interactive: ansi.IsTerminal(os.Stdin) && ansi.IsTerminal(os.Stderr),
	}, section)
}

// knownProfile errors unless profile is in the provider registry.
func knownProfile(profile string) error {
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("load providers: %w", err)
	}
	if reg.Providers[profile] == nil {
		return fmt.Errorf("unknown profile %s (see configure provider --list)", profile)
	}
	return nil
}

// configureRender runs --restore / --revert; the agent flag is a comma list there.
func configureRender(restore, revert bool, section, agents, out string, check, dryRun, renderAgents, noAgents bool) int {
	switch {
	case restore && revert:
		return fail("configure: --restore and --revert are mutually exclusive")
	case section != "":
		return fail("configure: --restore/--revert take no page (got %q)", section)
	case revert && (check || out != "" || renderAgents || noAgents):
		return fail("configure: --revert takes only --agent and --dry-run")
	case restore && agents != "" && !renderAgents:
		return fail("configure: agent-config rollback moved to --revert (--restore --agent needs --agents)")
	}
	// the engine skips unknown names silently, so a typo would render nothing; reject it here
	for a := range strings.SplitSeq(agents, ",") {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(agent.DefaultAgents, a) {
			return fail("configure: unknown --agent %q (%s)", a, strings.Join(agent.DefaultAgents, "|"))
		}
	}
	if revert {
		return cmdRevert(agents, dryRun)
	}
	if check && dryRun {
		return fail("configure: --check and --dry-run are mutually exclusive")
	}
	if noAgents && (renderAgents || agents != "") {
		return fail("configure: --no-agents excludes --agents and --agent")
	}
	return cmdRestore(agents, out, check, dryRun, renderAgents, noAgents)
}

func init() {
	// An agent the harness cannot route gets one interactive setup offer before the plain-CLI passthrough.
	agent.OfferConfigure = func(name string, reason error) bool {
		if !ansi.IsTerminal(os.Stdin) || !ansi.IsTerminal(os.Stderr) {
			return false
		}
		fmt.Fprintln(os.Stderr, reason)
		if !tui.Confirm("Set up "+name+" now?", true) {
			return false
		}
		changed, err := menu.Agent(name, true, os.Stderr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "configure %s: %v\n", name, err)
		}
		return changed
	}
}

var desktopPage = helptext.Page{
	Usage: "claude-code configure desktop [--profile p] [--revert] [--dry-run]",
	About: "Point Claude Desktop at the loopback proxy using a Claude-3p profile.\n" +
		"Open Desktop once first, then quit it before applying or reverting.\n" +
		"On WSL, targets Windows Desktop and runs the credential helper in WSL.\n" +
		"--revert restores the previous profile selection. --dry-run writes nothing\n" +
		"and prints exact destination files with custom header values masked.",
}

var (
	desktopRoot   = desktop.Root
	desktopHelper = desktop.HelperSpec
	desktopEnsure = proxy.Ensure
)

func cmdConfigureDesktop(args []string) int {
	fs := helptext.NewFlagSet("configure desktop", desktopPage)
	profile := fs.String("profile", "", "profile serving Desktop (default: session profile)")
	revert := fs.Bool("revert", false, "remove our Desktop profile and restore the previous selection")
	dryRun := fs.Bool("dry-run", false, "print exact files with secrets masked; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 0 {
		return fail("configure desktop: unexpected argument %q", fs.Arg(0))
	}
	if *revert && *profile != "" {
		return fail("configure desktop: --profile cannot be combined with --revert")
	}
	root, err := desktopRoot()
	if err != nil {
		return fail("configure desktop: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fail("configure desktop: open Claude Desktop once first (missing directory %s)", root)
	}
	opts := desktop.Options{Root: root, Revert: *revert, DryRun: *dryRun}
	if !*revert {
		reg, err := providers.Load()
		if err != nil {
			return fail("configure desktop: %v", err)
		}
		sel, err := providers.Select(reg, *profile)
		if err != nil {
			return fail("configure desktop: %v", err)
		}
		p := sel.Provider
		if p.Auth.Method() == providers.AuthCLI {
			return fail("configure desktop: profile %s uses a CLI login; select an API profile", p.Name)
		}
		opts.Profile = p.Name
		opts.HelperPath, opts.HelperArgs, opts.HelperWindows, err = desktopHelper(p.Name)
		if err != nil {
			return fail("configure desktop: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		opts.Models, err = desktop.ModelList(ctx, reg, p)
		if err != nil {
			return fail("configure desktop: %v", err)
		}
		opts.Headers = proxy.HarnessHeaders()
		opts.Headers[proxy.AgentHeader] = "desktop"
		base := proxy.Base(proxy.DefaultPort)
		if *dryRun {
			if st, ok := proxy.Running(ctx); ok {
				base = proxy.Base(st.Port)
			}
		} else {
			base, err = desktopEnsure(ctx)
			if err != nil {
				return fail("configure desktop: proxy unavailable; run gateway proxy --start")
			}
		}
		opts.BaseURL = proxy.SessionBase(base, p.Name)
	}
	if err := desktop.Configure(opts, os.Stdout); err != nil {
		return fail("configure desktop: %v", err)
	}
	return 0
}
