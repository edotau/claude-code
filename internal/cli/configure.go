package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/menu"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// hookSpec is the completion node for the hook verb.
func hookSpec() *helptext.Node { return &helptext.Node{Args: []func() []string{settings.HookEvents}} }

// hookPage lists modules in Hint, not Commands: a COMMANDS row would invite `hook <name> -h`, which runs the hook.
func hookPage() helptext.Page {
	return helptext.Page{
		Usage: "claude-code hook <Event> [--<module>|--all] < payload.json",
		About: "Run one lifecycle hook on the event JSON on stdin; settings.json and the agent hook files invoke it.",
		Hint: settings.HookModules() + "\n" + settings.HookHelp("events:  ", settings.HookEvents()) +
			"\n<Event> alone runs the dispatcher; --<module> one module; --all every module",
	}
}

// cmdHook runs one lifecycle hook.
func cmdHook(args []string) int {
	if code, ok := helpOrBare(hookPage(), args); ok {
		return code
	}
	return hooks.RunArgs(args[0], args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// configurePage: --profile picks the profile that serves model calls; --agent scopes to one agent CLI's own setup.
var configurePage = helptext.Page{
	Usage: "claude-code configure [<page>] [flags]\n" +
		"claude-code configure --restore|--revert [flags]\n" +
		"claude-code configure provider --all [<profile>] [--check]",
	About: "Guided setup for the profile, model pins and each agent CLI; bare opens a menu on a terminal " +
		"(agents, tools, settings), else walks every page in order. " +
		"--restore resets settings.json to the template (backup first), then re-renders the agent configs; " +
		"with --agents or --agent it renders only the agent configs and leaves settings.json alone. " +
		"--revert puts agent configs back from their backups. --format json applies to provider --list " +
		"and --all only. With --restore or --revert, --agent is a comma list without claude.",
	Commands: [][2]string{
		{"provider", "pick the session profile; --list shows credential state"},
		{"models", "pin models (live catalog: claude-code models --live)"},
		{"agents", "set up each agent CLI's routing (--agent for one)"},
		{"github", "GitHub Copilot logins and opencode's fallback ladder"},
		{"remote", "tailscale connectivity and remote hosts (read-only)"},
		{"codex", "Codex preferences with encrypted config history"},
		{"routing", "price weight, served-aware switch, opencode sub-pick knobs"},
		{"databricks", "browser sign-in and token mint per Databricks profile"},
	},
}

// configureOpts are cmdConfigure's flag values.
type configureOpts struct {
	provider, agent, out, auth                       string
	authSet                                          bool
	dryRun, check, restore, revert, agents, noAgents bool
	list, all                                        bool
	format                                           *string
}

func configureFlags() (*flag.FlagSet, *configureOpts) {
	fs := helptext.NewFlagSet("configure", configurePage)
	o := &configureOpts{}
	fs.StringVar(&o.provider, "profile", "", "select the session profile non-interactively")
	fs.StringVar(&o.agent, "agent", "", "set up one CLI: claude|codex|copilot|opencode|gemini")
	fs.StringVar(&o.auth, "auth", "", "with github: pin a GitHub account (login), or follow")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print what would change; write nothing (not with --restore)")
	fs.BoolVar(&o.check, "check", false, "report only; exit 1 if any unset (no --restore/--revert)")
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

// cmdConfigure is the guided setup: every section in order, or the one named; list prints `provider --list`.
func cmdConfigure(args []string) int {
	fs, o := configureFlags()
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
	if err := checkAuthFlag(fs, o, section); err != nil {
		return helptext.Fail("configure: %v", err)
	}
	return configureRun(o, section, pos)
}

// checkAuthFlag records whether --auth was passed (an empty value is meaningful) and refuses it off the github page.
func checkAuthFlag(fs *flag.FlagSet, o *configureOpts, section string) error {
	fs.Visit(func(f *flag.Flag) { o.authSet = o.authSet || f.Name == "auth" })
	if o.authSet && section != "github" {
		return errors.New("--auth needs the github page")
	}
	if o.authSet && (o.restore || o.revert) {
		return errors.New("--auth cannot be combined with --restore or --revert")
	}
	return nil
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

func configureList(section string, pos []string, format string) int {
	if section != "provider" {
		return helptext.Fail("configure: --list needs the provider page")
	}
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	return providerList(name, format)
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
		return configureList(section, pos, *o.format)
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
		Provider: o.provider, Agent: o.agent, Auth: o.auth, AuthSet: o.authSet, DryRun: o.dryRun, Check: o.check, All: o.all, Profile: profile,
		JSON: *o.format == helptext.JSON, Interactive: proc.IsTerminal(os.Stdin) && proc.IsTerminal(os.Stderr),
	}, section)
}

func init() {
	// An agent the harness cannot route gets one interactive setup offer before the plain-CLI passthrough.
	agent.OfferConfigure = func(name string, reason error) bool {
		if !proc.IsTerminal(os.Stdin) || !proc.IsTerminal(os.Stderr) {
			return false
		}
		fmt.Fprintln(os.Stderr, reason)
		if !tui.Confirm("Set up "+name+" now?", true) {
			return false
		}
		changed, err := menu.Agent(name, os.Stderr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "configure %s: %v\n", name, err)
		}
		return changed
	}
}

// configureRender runs --restore / --revert; the agent flag is a comma list there.
func configureRender(restore, revert bool, section, agents, out string, check, dryRun, renderAgents, noAgents bool) int {
	if err := renderModeConflict(restore, revert, section); err != nil {
		return helptext.Fail("%v", err)
	}
	if revert {
		return configureRevert(agents, dryRun, check || out != "" || renderAgents || noAgents)
	}
	if check || dryRun {
		return helptext.Fail("configure: --restore takes no --check or --dry-run")
	}
	return configureRestore(agents, out, renderAgents, noAgents)
}

func configureRestore(agents, out string, renderAgents, noAgents bool) int {
	if err := unknownAgent(agents); err != nil {
		return helptext.Fail("%v", err)
	}
	if msg := restoreConflict(agents, out, renderAgents, noAgents); msg != "" {
		return helptext.Fail("%s", msg)
	}
	if err := configure.Restore(os.Stdout, agents, out, renderAgents, noAgents); err != nil {
		return helptext.Fail("%v", err)
	}
	return 0
}

// restoreConflict names the --restore flag combination that cannot run, or "".
func restoreConflict(agents, out string, renderAgents, noAgents bool) string {
	agentsGiven := renderAgents || agents != ""
	switch {
	case noAgents && agentsGiven:
		return "configure: --no-agents excludes --agents and --agent"
	case out != "" && agentsGiven:
		return "configure: --out resets a settings file; --agents and --agent never write one"
	}
	return ""
}

// configureRevert runs --revert; extras is true when a flag that only --restore takes was given.
func configureRevert(agents string, dryRun, extras bool) int {
	if extras {
		return helptext.Fail("configure: --revert takes only --agent and --dry-run")
	}
	if err := unknownAgent(agents); err != nil {
		return helptext.Fail("%v", err)
	}
	if err := configure.Revert(os.Stdout, agents, dryRun); err != nil {
		return helptext.Fail("configure --revert: %v", err)
	}
	return 0
}

func renderModeConflict(restore, revert bool, section string) error {
	switch {
	case restore && revert:
		return errors.New("configure: --restore and --revert are mutually exclusive")
	case section != "":
		return fmt.Errorf("configure: --restore/--revert take no page (got %q)", section)
	}
	return nil
}

// unknownAgent rejects a typo before the engine runs: off ~/.claude it returns before its own name check.
func unknownAgent(agents string) error {
	for a := range strings.SplitSeq(agents, ",") {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(agent.DefaultAgents, a) {
			return fmt.Errorf("configure: unknown --agent %q (%s)", a, strings.Join(agent.DefaultAgents, "|"))
		}
	}
	return nil
}

// agentNames are the agent CLIs `configure --agent` can scope to: the agents plus claude itself.
func agentNames() []string { return append([]string{"claude"}, agent.DefaultAgents...) }

// authNames are the GitHub accounts, plus "follow", for `configure github --auth`.
func authNames() []string {
	reg, err := providers.Load()
	if err != nil {
		return []string{"follow"}
	}
	return append([]string{"follow"}, menu.GitHubAccounts(reg)...)
}

// sections derives from menu.SectionNames, the package's own run order.
func sections() []string {
	return append([]string{}, menu.SectionNames...)
}

// configureSpec is the completion node for the configure verb.
func configureSpec() *helptext.Node {
	fs, _ := configureFlags()
	return &helptext.Node{Args: []func() []string{sections, providers.ProfileNames}, Flags: helptext.FlagsOf(fs,
		map[string]func() []string{"profile": providers.ProfileNames, "agent": agentNames, "auth": authNames})}
}
