package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/cli/menu"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/cli/ui/tui"
	"github.com/edotau/claude-code/internal/cli/vscode"
	"github.com/edotau/claude-code/internal/dev/configure"
	"github.com/edotau/claude-code/internal/dev/desktop"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// hookSpec is the completion node for the hook verb.
func hookSpec() *helptext.Node { return &helptext.Node{Args: []func() []string{settings.HookEvents}} }

// hookPage lists modules in Hint, not Commands: a COMMANDS row would invite `hook <name> -h`, which runs the hook.
func hookPage() helptext.Page {
	return helptext.Page{
		Usage: "claude-code hook <Event> [--<module>|--all|--async] < payload.json",
		About: "Run one lifecycle hook on the event JSON on stdin; settings.json and the agent hook files invoke it.",
		Hint: settings.HookModules() + "\n" + settings.HookHelp("events:  ", settings.HookEvents()) +
			"\n<Event> alone runs the dispatcher; --<module> one module; --all every module",
	}
}

// cmdHook runs one lifecycle hook.
func cmdHook(args []string) int {
	if len(args) == 0 || isHelpVerb(args[0]) {
		if code, ok := helpOrBare(hookPage(), args); ok {
			return code
		}
	}
	return hooks.RunArgs(args[0], args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// configurePage: --agent scopes to one agent CLI's own setup (all: every agent CLI).
var configurePage = helptext.Page{
	Usage: "claude-code configure [<page>] [flags]\n" +
		"claude-code configure restore [--agent <name|all>] [--out FILE]\n" +
		"claude-code configure revert [--agent <name|all>] [--dry-run]\n" +
		"claude-code configure fmt [--check] [--out FILE]\n" +
		"claude-code configure agents <agent> key=value... [--dry-run]\n" +
		"claude-code configure routing <key>=<value>... [--dry-run]\n" +
		"claude-code configure provider --all [<profile>] [--check]\n" +
		"claude-code configure provider <profile> options.<key>=<value>...",
	About: "Guided setup for the profile, model pins and each agent CLI; bare opens a menu on a terminal " +
		"(agents, tools, settings), else walks every page in order. " +
		"restore resets settings.json to the template (backup first), then re-renders the agent configs; " +
		"with --agent it renders only the agent configs and leaves settings.json alone (all: every one). " +
		"revert puts agent configs back from their backups. fmt reorders settings.json (or --out) to schemastore " +
		"order and sorts env and string lists; fmt --check writes nothing and exits 1 at the first misplaced path. " +
		"With restore or revert, --agent is a comma list without claude. " +
		"provider --format json applies to --all; claude-code vault ls providers shows credential state; " +
		"switch <profile> picks the session profile.",
	Commands: [][2]string{
		{"provider", "pick the session profile"},
		{"models", "pin models (live catalog: claude-code models --live)"},
		{"agents", "set up each agent CLI's routing (--agent for one)"},
		{"github", "GitHub Copilot logins and opencode's fallback ladder"},
		{"remote", "tailscale connectivity and remote hosts (read-only)"},
		{"desktop", "Claude Desktop loopback profile; --revert restores selection"},
		{"codex", "Codex preferences with encrypted config history"},
		{"routing", "weights, spend caps, served-aware and opencode knobs"},
		{"databricks", "browser sign-in and token mint per Databricks profile"},
		{"theme", "status line palette, tool themes (see theme -h)"},
		{"vscode", "merge VS Code settings, keybindings, chat models"},
		{"history", "config snapshots: snapshot, log, diff (see history -h)"},
		{"restore", "reset settings.json to the template (backup first)"},
		{"revert", "put agent configs back from their backups"},
		{"fmt", "reorder settings.json to schemastore order (--check: report)"},
	},
}

// configureOpts are cmdConfigure's flag values; restore, revert and allAgents come from the page word and --agent all.
type configureOpts struct {
	agent, out                     string
	dryRun, check, restore, revert bool
	allAgents, all                 bool
	format                         *string
}

func configureFlags() (*flag.FlagSet, *configureOpts) {
	fs := helptext.NewFlagSet("configure", configurePage)
	o := &configureOpts{}
	fs.StringVar(&o.agent, "agent", "", "set up one CLI: claude|codex|copilot|opencode|gemini, or all")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print what would change; write nothing (not with restore)")
	fs.BoolVar(&o.check, "check", false, "report only; exit 1 if any unset (not with restore|revert)")
	fs.StringVar(&o.out, "out", "", "with restore|fmt: act on this file, not settings.json")
	fs.BoolVar(&o.all, "all", false, "with provider: check every profile's sign-in and log in where needed")
	o.format = helptext.FormatFlag(fs)
	return fs, o
}

// removedForms maps each retired flag to the form that replaces it.
var removedForms = map[string]string{
	"--history": "configure history", "--theme": "configure theme", "--vscode": "configure vscode",
	"--restore": "configure restore", "--revert": "configure revert", "--fmt": "configure fmt",
	"--agents": "configure restore --agent all", "--profile": "switch <profile>", "--list": "vault ls providers",
}

// removedForm returns the first retired flag in args and its replacement, or "".
func removedForm(args []string) (arg, use string) {
	for _, a := range args {
		if use, ok := removedForms[strings.SplitN(a, "=", 2)[0]]; ok {
			return a, use
		}
	}
	return "", ""
}

// cmdConfigure is the guided setup: every section in order, or the one named.
func cmdConfigure(args []string) int {
	if code, ok := configureRoute(args); ok {
		return code
	}
	fs, o := configureFlags()
	// `configure agents --agent codex`: a leading section word precedes the flags, so lift it out first.
	section, args := leadingID(args)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if section == "" && len(pos) > 0 {
		section, pos = pos[0], pos[1:]
	}
	if section == "routing" && len(pos) > 0 {
		invalid := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "dry-run" && f.Name != "check" {
				invalid = true
			}
		})
		if invalid {
			return helptext.FailUsage("configure routing: assignments take only --dry-run or --check")
		}
	}
	o.setMode(section)
	if section == "fmt" {
		return configureFmt(fs, o, pos)
	}
	return configureRun(o, section, pos)
}

// setMode derives restore, revert and allAgents from the page word and --agent all.
func (o *configureOpts) setMode(section string) {
	if o.allAgents = o.agent == "all"; o.allAgents {
		o.agent = ""
	}
	o.restore, o.revert = section == "restore", section == "revert"
}

// configureRoute runs `desktop|history|theme|vscode ...` or refuses a retired flag; ok is false otherwise.
func configureRoute(args []string) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "desktop":
		return configureDesktop(args[1:]), true
	case "history":
		return cmdHistory(args[1:]), true
	case "theme":
		return cmdTheme(args[1:]), true
	case "vscode":
		return vscode.Run(args[1:]), true
	}
	if arg, use := removedForm(args); arg != "" {
		return helptext.FailUsage("configure: %s is removed; use `claude-code %s`", arg, use), true
	}
	return 0, false
}

// configureAllProfile validates --all's combinations and returns its optional <profile>.
func configureAllProfile(o *configureOpts, section string, pos []string) (string, error) {
	if !o.all {
		return "", nil
	}
	switch {
	case section != "provider":
		return "", errors.New("--all needs the provider page")
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
		return helptext.Fail("configure: --format needs the provider page (listing: claude-code vault ls providers)")
	}
	name := ""
	if len(pos) > 0 {
		name = pos[0]
	}
	return providerList(name, format)
}

// listsInstead is true for a non-text --format outside --all, restore and revert.
func (o *configureOpts) listsInstead() bool {
	return *o.format != helptext.Text && !o.all && !o.restore && !o.revert
}

// isAgentRecord is true for `configure agents <name> key=value ...`.
func isAgentRecord(section string, pos []string) bool {
	return section == "agents" && len(pos) >= 2 && strings.Contains(pos[1], "=")
}

// configureAssignment runs `configure agents|provider <name> key=value ...`; ok is false for any other shape.
func configureAssignment(o *configureOpts, section string, pos []string) (rc int, ok bool) {
	switch {
	case section == "routing" && len(pos) > 0:
		return configureRoutingSet(pos, o.dryRun || o.check), true
	case isAgentRecord(section, pos):
		return configureRecord(pos[0], pos[1:], o.dryRun || o.check), true // --check never writes
	case section == "provider" && len(pos) >= 2 && strings.Contains(pos[1], "="):
		return configureProviderOptions(o, pos), true
	}
	return 0, false
}

// configureRoutingSet stages a typed fixed-key patch, applied in module order as SaveRouting does, so preview matches.
func configureRoutingSet(pairs []string, dryRun bool) int {
	fields := []configure.RoutingField{}
	for _, m := range configure.Modules() {
		fields = append(fields, m.Fields()...)
	}
	staged, resets, order, err := stageRoutingPairs(fields, pairs)
	if err != nil {
		return helptext.FailUsage("%v", err)
	}
	r, err := providers.RoutingOverlay()
	if err != nil {
		return helptext.Fail("configure routing: %v", err)
	}
	if err := applyRouting(&r, fields, staged); err != nil {
		return helptext.FailUsage("configure routing: %v", err)
	}
	doc, err := json.Marshal(map[string]providers.Routing{"routing": r})
	if err != nil {
		return helptext.FailUsage("configure routing: %v", err)
	}
	if err := providers.Validate(doc); err != nil {
		return helptext.FailUsage("configure routing: %v", err)
	}
	if !dryRun {
		if err := configure.SaveRouting(staged, resets); err != nil {
			return helptext.Fail("configure routing: %v", err)
		}
	}
	for _, f := range order {
		fmt.Fprintf(os.Stdout, "routing %s=%s\n", f.Key, f.Get(r))
	}
	return 0
}

// applyRouting sets each staged field on r, in module order.
func applyRouting(r *providers.Routing, fields []configure.RoutingField, staged map[string]string) error {
	for _, f := range fields {
		if value, ok := staged[f.Key]; ok {
			if err := f.Set(r, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// stageRoutingPairs parses key=value words; the reset flags mark empty values, order is first-seen.
func stageRoutingPairs(fields []configure.RoutingField, pairs []string) (map[string]string, map[string]bool, []configure.RoutingField, error) {
	staged, resets := map[string]string{}, map[string]bool{}
	var order []configure.RoutingField
	var known []string
	for _, f := range fields {
		known = append(known, f.Key)
	}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, nil, nil, fmt.Errorf("configure routing: %q must be key=value", pair)
		}
		i := slices.IndexFunc(fields, func(f configure.RoutingField) bool { return f.Key == key })
		if i < 0 {
			return nil, nil, nil, fmt.Errorf("configure: unknown routing key %q (known: %s)", key, strings.Join(known, ", "))
		}
		if err := configure.ValidateRoutingInput(fields[i], value); err != nil {
			return nil, nil, nil, fmt.Errorf("configure routing: %v", err)
		}
		if _, seen := staged[key]; !seen {
			order = append(order, fields[i])
		}
		staged[key], resets[key] = value, value == ""
	}
	return staged, resets, order, nil
}

// liftAgentName makes `configure agents <agent>` mean --agent <agent>, so one agent is set up, not all.
func liftAgentName(o *configureOpts, section string, pos []string) error {
	if section != "agents" || len(pos) == 0 {
		return nil
	}
	if len(pos) > 1 || o.agent != "" && o.agent != pos[0] {
		return fmt.Errorf("agents takes one <agent> (got %q with --agent %q)", strings.Join(pos, " "), o.agent)
	}
	o.agent = pos[0]
	return nil
}

func configureRun(o *configureOpts, section string, pos []string) int {
	if rc, ok := configureAssignment(o, section, pos); ok {
		return rc
	}
	if err := liftAgentName(o, section, pos); err != nil {
		return helptext.Fail("configure: %v", err)
	}
	profile, err := configureAllProfile(o, section, pos)
	if err != nil {
		return helptext.Fail("configure: %v", err)
	}
	if o.listsInstead() {
		return configureList(section, pos, *o.format)
	}
	if o.restore || o.revert {
		return configureRender(o, section, pos)
	}
	if o.out != "" {
		return helptext.Fail("configure: --out needs restore or fmt")
	}
	return configureMenu(o, section, profile)
}

func configureMenu(o *configureOpts, section, profile string) int {
	return menu.Run(menu.Options{
		Agent: o.agent, DryRun: o.dryRun, Check: o.check, All: o.all, Profile: profile,
		JSON: *o.format == helptext.JSON, Interactive: proc.IsTerminal(os.Stdin) && proc.IsTerminal(os.Stderr),
	}, section)
}

func wireConfigure() {
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

// configureFmt is the owner-run `configure fmt [--check]`: nothing else rewrites settings.json's order.
func configureFmt(fs *flag.FlagSet, o *configureOpts, pos []string) int {
	invalid := len(pos) > 0
	fs.Visit(func(f *flag.Flag) {
		if !slices.Contains([]string{"check", "out"}, f.Name) {
			invalid = true
		}
	})
	if invalid {
		return helptext.Fail("configure: fmt takes only --check and --out")
	}
	file := o.out
	if file == "" {
		file = settings.SettingsFile()
	}
	if o.check {
		data, err := os.ReadFile(file)
		if err != nil {
			return helptext.Fail("configure fmt: %v", err)
		}
		path, err := settings.Unformatted(data)
		if err != nil {
			return helptext.Fail("configure fmt: %s: %v", file, err)
		}
		if path != "" {
			fmt.Fprintf(os.Stderr, "%s: out of order at %s\n", file, path)
			return 1
		}
		fmt.Fprintf(os.Stdout, "%s: already formatted\n", file)
		return 0
	}
	backup, err := settings.FormatFile(file)
	if err != nil {
		return helptext.Fail("configure fmt: %v", err)
	}
	if backup != "" {
		fmt.Fprintf(os.Stdout, "%s: reordered to schema order; backup at %s\n", file, backup)
	} else {
		fmt.Fprintf(os.Stdout, "%s: already formatted\n", file)
	}
	return 0
}

var desktopPage = helptext.Page{
	Usage: "claude-code configure desktop [--profile p] [--revert] [--dry-run]",
	About: "Point Claude Desktop at the loopback proxy using a Claude-3p profile.\n" +
		"Open Desktop once first, then quit it before applying or reverting.\n" +
		"On WSL, targets Windows Desktop and runs the credential helper in WSL.\n" +
		"--revert restores the previous profile selection and revokes the desktop\n" +
		"token. --dry-run writes nothing and prints exact destination files with\n" +
		"custom header values masked.",
}

var (
	desktopRoot   = desktop.Root
	desktopHelper = desktop.HelperSpec
	desktopEnsure = proxy.Ensure
)

func configureDesktop(args []string) int {
	fs := helptext.NewFlagSet("configure desktop", desktopPage)
	profile := fs.String("profile", "", "profile serving Desktop (default: session profile)")
	revert := fs.Bool("revert", false, "remove our profile; restore the previous selection")
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
	if err := desktop.CheckRoot(root, !*revert && !*dryRun); err != nil {
		return helptext.Fail("configure desktop: %v", err)
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

// configureRender runs restore / revert; the agent flag is a comma list there.
func configureRender(o *configureOpts, section string, pos []string) int {
	if len(pos) > 0 {
		return helptext.Fail("configure %s: unexpected argument %q", section, pos[0])
	}
	if o.revert {
		return configureRevert(o.agent, o.dryRun, o.check || o.out != "")
	}
	if o.check || o.dryRun {
		return helptext.Fail("configure: restore takes no --check or --dry-run")
	}
	return configureRestore(o.agent, o.out, o.allAgents)
}

func configureRestore(agents, out string, renderAgents bool) int {
	if err := unknownAgent(agents); err != nil {
		return helptext.Fail("%v", err)
	}
	if msg := restoreConflict(agents, out, renderAgents); msg != "" {
		return helptext.Fail("%s", msg)
	}
	if err := configure.Restore(os.Stdout, agents, out, renderAgents); err != nil {
		return helptext.Fail("%v", err)
	}
	return 0
}

// restoreConflict names the restore flag combination that cannot run, or "".
func restoreConflict(agents, out string, renderAgents bool) string {
	if out != "" && (renderAgents || agents != "") {
		return "configure: --out resets a settings file; --agent never writes one"
	}
	return ""
}

// configureRevert runs revert; extras is true when a flag that only restore takes was given.
func configureRevert(agents string, dryRun, extras bool) int {
	if extras {
		return helptext.Fail("configure: revert takes only --agent and --dry-run")
	}
	if err := unknownAgent(agents); err != nil {
		return helptext.Fail("%v", err)
	}
	if err := configure.Revert(os.Stdout, agents, dryRun); err != nil {
		return helptext.Fail("configure revert: %v", err)
	}
	return 0
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

// agentNames are the values `configure --agent` takes: claude, the agents, and all.
func agentNames() []string { return append([]string{"claude", "all"}, agent.DefaultAgents...) }

// sections derives from menu.SectionNames, the package's own run order.
func sections() []string {
	return append(append([]string{}, menu.SectionNames...), "desktop")
}

// configureSpec is the completion node for the configure verb.
func configureSpec() *helptext.Node {
	fs, _ := configureFlags()
	flags := helptext.FlagsOf(fs, map[string]func() []string{"agent": agentNames})
	return &helptext.Node{Args: []func() []string{sections, providers.ProfileNames}, Flags: flags,
		Sub: map[string]*helptext.Node{"history": historySpec(), "theme": themeSpec(), "vscode": vscode.Spec(),
			"restore": {Flags: flags}, "revert": {Flags: flags}, "fmt": {Flags: flags}}}
}
