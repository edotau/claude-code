package configurecmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
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
		"--restore resets settings.json to the template (backup first), then re-renders the agent configs; " +
		"with --agents or --agent it renders only the agent configs and leaves settings.json alone. " +
		"--revert puts agent configs back from their backups.",
	Commands: [][2]string{
		{"provider", "pick the session profile; --list shows credential state"},
		{"models", "pin models (live catalog: claude-code models --live)"},
		{"agents", "set up each agent CLI's routing (--agent for one)"},
		{"github", "GitHub Copilot logins and opencode's fallback ladder"},
		{"remote", "tailscale connectivity and remote hosts (read-only)"},
		{"desktop", "Claude Desktop loopback profile; --revert restores selection"},
		{"codex", "Codex preferences with encrypted config history"},
		{"routing", "price weight, subagent offload, opencode's sub-pick knobs"},
		{"databricks", "browser sign-in and token mint per Databricks profile"},
	},
}

// configureOpts are cmdConfigure's flag values.
type configureOpts struct {
	provider, agent, out                             string
	dryRun, check, restore, revert, agents, noAgents bool
	list, all                                        bool
	format                                           *string
	providerList                                     func(name, format string) int
}

func configureFlags() (*flag.FlagSet, *configureOpts) {
	fs := helptext.NewFlagSet("configure", configurePage)
	o := &configureOpts{}
	fs.StringVar(&o.provider, "profile", "", "select the session profile non-interactively")
	fs.StringVar(&o.agent, "agent", "", "set up one CLI: claude|codex|copilot|opencode|gemini")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print what would change; write nothing")
	fs.BoolVar(&o.check, "check", false, "report only; exit 1 if anything is unset")
	fs.BoolVar(&o.restore, "restore", false, "reset settings.json to the template (backup first)")
	fs.BoolVar(&o.revert, "revert", false, "put agent configs back from their timestamped backups")
	fs.StringVar(&o.out, "out", "", "with --restore: reset this file instead of settings.json")
	fs.BoolVar(&o.agents, "agents", false, "with --restore: agent configs only, even off ~/.claude")
	fs.BoolVar(&o.noAgents, "no-agents", false, "with --restore: settings.json only, skip the agent configs")
	fs.BoolVar(&o.list, "list", false, "with provider: list profiles and credential state")
	fs.BoolVar(&o.all, "all", false, "with provider: check every profile's sign-in and log in where needed")
	o.format = helptext.FormatFlag(fs)
	return fs, o
}

// Run is the guided setup: every section in order, or the one named; providerList prints `provider --list`.
func Run(args []string, providerList func(name, format string) int) int {
	if len(args) > 0 && args[0] == "desktop" {
		return configureDesktop(args[1:])
	}
	fs, o := configureFlags()
	o.providerList = providerList
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
	return configureRun(o, section, pos)
}

// configureAllProfile validates --all's combinations and returns its optional <profile>.
func configureAllProfile(o *configureOpts, section string, pos []string) (string, error) {
	if !o.all {
		return "", nil
	}
	switch {
	case section != "provider":
		return "", errors.New("--all needs the provider page")
	case o.list || o.provider != "":
		return "", errors.New("--all cannot be combined with --list or --profile")
	case len(pos) > 1:
		return "", fmt.Errorf("--all takes at most one <profile> (got %d)", len(pos))
	case len(pos) == 0:
		return "", nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", fmt.Errorf("load providers: %w", err)
	}
	return pos[0], reg.Known(pos[0])
}

func configureList(o *configureOpts, section string, pos []string, format string) int {
	if section != "provider" {
		return helptext.Fail("configure: --list needs the provider page")
	}
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	return o.providerList(name, format)
}

// wantsList is true for --list, or a non-text --format outside the --all check.
func (o *configureOpts) wantsList() bool {
	return o.list || *o.format != helptext.Text && !o.all
}

func configureRun(o *configureOpts, section string, pos []string) int {
	profile, err := configureAllProfile(o, section, pos)
	if err != nil {
		return helptext.Fail("configure: %v", err)
	}
	if o.wantsList() {
		return configureList(o, section, pos, *o.format)
	}
	if o.restore || o.revert {
		return configureRender(o.restore, o.revert, section, o.agent, o.out, o.check, o.dryRun, o.agents, o.noAgents)
	}
	if o.out != "" || o.agents || o.noAgents {
		return helptext.Fail("configure: --out, --agents and --no-agents need --restore")
	}
	return configureMenu(o, section, profile)
}

func configureMenu(o *configureOpts, section, profile string) int {
	return menu.Run(menu.Options{
		Provider: o.provider, Agent: o.agent, DryRun: o.dryRun, Check: o.check, All: o.all, Profile: profile,
		JSON: *o.format == helptext.JSON, Interactive: ansi.IsTerminal(os.Stdin) && ansi.IsTerminal(os.Stderr),
	}, section)
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

func configureDesktop(args []string) int {
	fs := helptext.NewFlagSet("configure desktop", desktopPage)
	profile := fs.String("profile", "", "profile serving Desktop (default: session profile)")
	revert := fs.Bool("revert", false, "remove our Desktop profile and restore the previous selection")
	dryRun := fs.Bool("dry-run", false, "print exact files with secrets masked; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 0 {
		return helptext.Fail("configure desktop: unexpected argument %q", fs.Arg(0))
	}
	if *revert && *profile != "" {
		return helptext.Fail("configure desktop: --profile cannot be combined with --revert")
	}
	root, err := desktopRoot()
	if err != nil {
		return helptext.Fail("configure desktop: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return helptext.Fail("configure desktop: open Claude Desktop once first (missing directory %s)", root)
	}
	opts := desktop.Options{Root: root, Revert: *revert, DryRun: *dryRun}
	return configureDesktopOptions(opts, *profile)
}

func configureDesktopOptions(opts desktop.Options, profile string) int {
	if !opts.Revert {
		reg, err := providers.Load()
		if err != nil {
			return helptext.Fail("configure desktop: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if opts, err = desktop.BuildOptions(ctx, reg, profile, opts.DryRun, opts, desktopHelper, desktopEnsure); err != nil {
			return helptext.Fail("configure desktop: %v", err)
		}
	}
	if err := desktop.Configure(opts, os.Stdout); err != nil {
		return helptext.Fail("configure desktop: %v", err)
	}
	return 0
}
