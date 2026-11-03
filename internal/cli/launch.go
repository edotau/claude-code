package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
	usagecmd "github.com/edotau/claude-code/internal/usage"
)

func cmdClaude(args []string) int { return launchClaude("", args) }

// launchClaude launches Claude Code with alias as the main-loop model ("" = opus).
func launchClaude(alias string, args []string) int {
	as, args, err := takeAs(args)
	if err != nil {
		return fail("%v", err)
	}
	opts, rest, err := parseLaunchArgs(args, true)
	if err != nil {
		return fail("%v", err)
	}
	opts.Alias = alias
	plan, err := claude.Claude(providers.WithIdentity(context.Background(), as), opts, rest)
	if err == nil && !opts.PrintEnv {
		agent.RecordLastProfile("claude", plan.Set[providers.EnvSessionProfile], rest)
	}
	return runPlan("claude", plan, opts, err)
}

// takeAs removes --as user|sp (or --as=V) from args and validates it; it stops at a bare "--".
func takeAs(args []string) (string, []string, error) {
	var as string
	var rest []string
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		switch {
		case args[i] == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case name != "--as":
			rest = append(rest, args[i])
		case hasVal:
			as = val
		case i+1 < len(args):
			i++
			as = args[i]
		default:
			return "", nil, errors.New("--as needs user or sp")
		}
	}
	if as != "" && as != providers.IdentityUser && as != providers.IdentitySP {
		return "", nil, fmt.Errorf("--as %q must be user or sp", as)
	}
	return as, rest, nil
}

// aliasVerb is `claude-code <alias>`: Claude Code on the session profile with that alias as the main model.
func aliasVerb(alias string) func([]string) int {
	return func(a []string) int { return launchClaude(alias, a) }
}

var anthropicPage = helptext.Page{
	Usage: "claude-code anthropic [--oauth[=NAME]] [--x-api-key[=NAME]] [--model M]\n" +
		"[--print-env] [--keep-env NAMES] [--overlay FILE] [claude args]",
	About: "Claude Code on first-party Anthropic. No flag uses your claude.ai login. A bare flag takes no " +
		"value; =NAME picks the vault key.",
	Hint: "claude-code anthropic " + helptext.DocsFlag + " prints claude's own help.",
}

// anthropicHelp renders the page with its flag rows; the flags are parsed by claude.ParseAnthropicArgs.
func anthropicHelp(w io.Writer) {
	fs := helptext.NewFlagSet("anthropic", anthropicPage)
	fs.Bool("oauth", false, "setup-token login; =NAME picks the vault key")
	fs.Bool("x-api-key", false, "API key login; =NAME picks the vault key")
	fs.String("model", "", "alias (opus), version (opus-5-5) or wire id: the main model")
	fs.Bool("print-env", false, "print the resolved launch; never prompts")
	fs.String("keep-env", "", "env names the credential scrub keeps")
	fs.String("overlay", "", "JSON {set,unset,keep} file over the child's env")
	p := anthropicPage
	p.Flags = fs
	p.Fprint(w)
}

// cmdAnthropic is `claude-code anthropic`: first-party Claude Code with a login, OAuth token or API key.
func cmdAnthropic(args []string) int {
	if helptext.WantsHelp(args) {
		anthropicHelp(os.Stdout)
		return 0
	}
	if len(args) == 1 && args[0] == helptext.DocsFlag {
		return helptext.WrappedDocs("claude")
	}
	if err := databricks.RemovedFlagHint(args); err != nil {
		return fail("%v", err)
	}
	o, rest, err := claude.ParseAnthropicArgs(args)
	if err != nil {
		return fail("%v", err)
	}
	secret := ""
	if o.Mode != claude.AuthLogin {
		if secret, err = anthropicSecret(o); err != nil {
			return fail("%v", err)
		}
	}
	plan, err := claude.Anthropic(context.Background(), o, rest, secret)
	return runPlan("claude", plan, o.LaunchOptions, err)
}

// anthropicSecret reads the mode's vault key; when it is missing and a terminal is attached, offers to store it.
func anthropicSecret(o claude.AnthropicOptions) (string, error) {
	read := func() (string, error) {
		if o.Mode == claude.AuthOAuth {
			return providers.StoredSecret(o.Name), nil
		}
		v, _, err := secretstore.EffectiveSecret(o.Name)
		return v, err
	}
	if v, err := read(); err != nil {
		return "", fmt.Errorf("%s: %w", o.Name, err)
	} else if v != "" {
		return v, nil
	}
	missing := fmt.Errorf("%s is not in the vault; store it with `claude-code vault set %s`", o.Name, o.Name)
	if o.PrintEnv || !tui.StdinIsTerminal() || !ansi.IsTerminal(os.Stderr) {
		return "", missing
	}
	if !tui.Confirm(o.Name+" is not in the vault — set it now?", true) {
		return "", missing
	}
	if o.Mode == claude.AuthOAuth && tui.Confirm("Run `claude setup-token` first to mint one?", true) {
		runSetupToken()
	}
	if code := vaultSetSecret(o.Name, false); code != 0 {
		return "", fmt.Errorf("%s was not stored", o.Name)
	}
	if v, err := read(); err != nil {
		return "", fmt.Errorf("%s: %w", o.Name, err)
	} else if v != "" {
		return v, nil
	}
	return "", missing
}

// runSetupToken runs `claude setup-token` on the inherited terminal; a failure only means the user pastes a token.
func runSetupToken() {
	bin, err := claude.ClaudeBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-code:", err)
		return
	}
	cmd := exec.CommandContext(context.Background(), bin, "setup-token")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: claude setup-token:", err)
	}
}

// parseLaunchArgs refuses a removed flag (with its hint) before agent.ParseArgs strips the harness flags.
func parseLaunchArgs(args []string, claude bool) (agent.LaunchOptions, []string, error) {
	if err := databricks.RemovedFlagHint(args); err != nil {
		return agent.LaunchOptions{}, nil, err
	}
	return agent.ParseArgs(args, claude)
}

func launchAgent(name string, args []string) int {
	opts, rest, err := parseLaunchArgs(args, false)
	if err != nil {
		return fail("%v", err)
	}
	plan, err := agent.LaunchPlan(context.Background(), name, opts, rest)
	if err == nil {
		warnVersion(name, plan.Binary, opts, rest)
		if !opts.PrintEnv {
			agent.RecordLastProfile(name, plan.Profile, rest)
		}
	}
	return runPlan(name, plan, opts, err)
}

// warnVersion prints the one-line out-of-bounds warning for an interactive launch of the agent binary at path.
func warnVersion(name, path string, opts agent.LaunchOptions, args []string) {
	if !agent.WantsVersionWarning(opts, args) || !agentVersionTTY() {
		return
	}
	if line := tools.VersionWarning(name, path); line != "" {
		fmt.Fprintln(versionWarnOut, line)
	}
}

// agentVersionTTY and versionWarnOut are the version warning's terminal gate and sink (tests swap them).
var (
	agentVersionTTY           = func() bool { return ansi.IsTerminal(os.Stderr) }
	versionWarnOut  io.Writer = os.Stderr
)

func cmdUsage(args []string) int { return usagecmd.Run(args, paths.ConfigDir(), os.Stdout, os.Stderr) }

// cmdScience takes only a leading --print-env; every other flag is claude-science's own.
func cmdScience(args []string) int {
	var opts agent.LaunchOptions
	if len(args) > 0 && args[0] == "--print-env" {
		opts.PrintEnv, args = true, args[1:]
	}
	plan, err := claude.ScienceEnv(args)
	return runPlan("claude-science", plan, opts, err)
}

func runPlan(name string, plan agent.Plan, opts agent.LaunchOptions, err error) int {
	plan.Agent = name
	if usage, ok := errors.AsType[*agent.UsageError](err); ok {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if err != nil {
		return fail("%v", err)
	}
	ov, oerr := launchOverlay(opts)
	if oerr != nil {
		if plan.OnExecFail != nil {
			plan.OnExecFail() // a minted session token dies with the refused launch
		}
		fmt.Fprintln(os.Stderr, "claude-code:", oerr)
		return 2
	}
	plan = plan.WithOverlay(ov)
	if opts.PrintEnv {
		plan.Print(os.Stdout)
		if agent.OverlayDebugOn() {
			plan.RecordEnv() // dry exec: the env Exec would hand the child
		}
		return 0
	}
	return fail("exec %s: %v", plan.Binary, plan.Exec())
}

// launchOverlay is the overlay for an interactive launch: --overlay, else $COWORKER_OVERLAY; --print-env keeps vault
// refs unresolved so it shows their names, never their values.
func launchOverlay(opts agent.LaunchOptions) (agentconfig.Resolved, error) {
	o, err := agentconfig.LoadOverlay(opts.Overlay)
	if err != nil {
		return agentconfig.Resolved{}, err
	}
	if opts.PrintEnv {
		return agentconfig.Resolved{Set: o.Set, Unset: o.Unset, Keep: o.Keep}, nil
	}
	return agentconfig.ResolveOverlay(o)
}

// hookModules wraps the module names from settings.Specs into help lines under 80 columns.
func hookModules() string {
	var lines []string
	line := "modules: "
	for _, sp := range settings.Specs() {
		if len(line)+len(sp.Name)+1 > 79 {
			lines = append(lines, line)
			line = "         "
		}
		line += sp.Name + "|"
	}
	return strings.Join(append(lines, strings.TrimSuffix(line, "|")), "\n")
}

// hookPage lists modules in Hint, not Commands: a COMMANDS row would invite `hook <name> -h`, which runs the hook.
var hookPage = helptext.Page{
	Usage: "claude-code hook <Event> [--<module>|--all] < payload.json",
	About: "Run one lifecycle hook on the event JSON on stdin; settings.json and the agent hook files invoke it.",
	Hint:  hookModules() + "\nevents: <Event> runs the dispatcher, --<module> one module, --all every module",
}

func cmdHook(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		hookPage.Fprint(os.Stderr)
		return 0
	}
	return hooks.RunArgs(args[0], args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// cmdRestore is `configure --restore`; the work lives in launch.Restore.
func cmdRestore(agents, out string, check, dryRun, forceHomes, noAgents bool) int {
	drift, err := launch.Restore(os.Stdout, agents, out, check, dryRun, forceHomes, noAgents)
	if err != nil {
		return fail("%v", err)
	}
	if drift {
		return 1
	}
	return 0
}

// cmdRevert handles `configure --revert [--agent a,b] [--dry-run]`.
func cmdRevert(agents string, dryRun bool) int {
	if err := launch.Revert(os.Stdout, agents, dryRun); err != nil {
		return fail("configure --revert: %v", err)
	}
	return 0
}

// toolsFlag is --tools, bare (every tool) or --tools=name,name2 (that subset only).
type toolsFlag struct {
	set   bool
	names []string
}

func (t *toolsFlag) String() string {
	if t == nil || !t.set {
		return ""
	}
	return strings.Join(t.names, ",")
}

func (t *toolsFlag) Set(s string) error {
	t.set = true
	if s != "" && s != "true" {
		t.names = strings.Split(s, ",")
	}
	return nil
}

func (t *toolsFlag) IsBoolFlag() bool { return true } // allows the bare "--tools" form

func cmdInstall(args []string) int {
	if len(args) > 0 && (args[0] == "--setup" || args[0] == "-setup") {
		return cmdSetup(args[1:])
	}
	fs := helptext.NewFlagSet("install", helptext.Page{
		Usage: "claude-code install [flags]\nclaude-code install --setup <mode> [flags]",
		About: "Build-time setup: install the Claude Code CLI and agent links, then render settings.json. " +
			"--tools installs only the agent CLIs; --setup migrates, wipes or scopes a .claude/ tree.",
		Hint: "claude-code install --setup -h lists the setup modes.",
	})
	dry := fs.Bool("dry-run", false, "print what would change; write nothing")
	verbose := fs.Bool("verbose", false, "print every step in full, instead of one line each")
	var tf toolsFlag
	fs.Var(&tf, "tools", "install CLIs only: bare = all; --tools=name,... selects tools")
	setup := fs.String("setup", "", "adopt|nuke|workspace: migrate, wipe, or scope .claude/ (first arg)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *setup != "" {
		return fail("install: --setup must come first: claude-code install --setup <mode> [flags]")
	}
	if tf.set {
		if err := tools.Install(context.Background(), os.Stdout, tf.names, *dry); err != nil {
			return fail("install --tools: %v", err)
		}
		return 0
	}
	if err := launch.Install(os.Stdout, *dry, *verbose); err != nil {
		return fail("install: %v", err)
	}
	return 0
}

// doctorChecks is a test seam over the slow, probing launch.DoctorChecks.
var doctorChecks = launch.DoctorChecks

func cmdDoctor(args []string) int {
	fs := helptext.NewFlagSet("doctor", helptext.Page{
		Usage: "claude-code doctor [flags] [--scan [path...]]",
		About: "Check binaries, providers, credentials and the proxy without changing anything; " +
			"exits 1 when a check fails. --audit prints switches, pins, secret sets, safety blocks " +
			"and proxy failovers instead. --scan, --git and --secrets audit for plaintext " +
			"credentials instead and exit 1 on any finding.",
	})
	format := helptext.FormatFlag(fs)
	auditMode := fs.Bool("audit", false, "print the typed action log instead of health checks")
	days := fs.Int("days", 7, "with --audit: events from the last N days")
	scan := fs.Bool("scan", false, "scan paths given, else configs and vault, for inline secrets")
	gitScan := fs.Bool("git", false, "scan this repo's git history for committed secrets")
	since := fs.Int("since", 0, "with --git or --secrets: only the last N commits (0 = all)")
	rev := fs.String("range", "", "with --git or --secrets: only this revision range, e.g. @{u}..HEAD")
	refresh := fs.Bool("refresh", false, "re-list each pinned Databricks workspace's served models before the checks")
	all := fs.Bool("secrets", false, "--scan, --git and the vault checks together; worst exit wins")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("doctor", err)
	}
	if len(fs.Args()) > 0 && !*scan {
		fail("doctor: unexpected arguments %v (paths go after --scan)", fs.Args())
		return 2
	}
	if *scan || *gitScan || *all {
		if *format == helptext.JSON {
			fail("doctor: --scan, --git and --secrets print text only")
			return 2
		}
		return secrets.RunAudits(os.Stdout, secrets.AuditOpts{Scan: *scan || *all, Git: *gitScan || *all, Vault: *all,
			Targets: fs.Args(), Rev: *rev, Since: *since, VaultDoctor: launch.VaultDoctor})
	}
	if *auditMode {
		return printAudit(*days, *format)
	}
	if *refresh {
		refreshServed()
	}
	checks, ok := doctorChecks()
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			OK     bool           `json:"ok"`
			Checks []launch.Check `json:"checks"`
		}{ok, checks}); err != nil {
			return fail("doctor: %v", err)
		}
	} else {
		launch.PrintChecks(os.Stdout, checks)
	}
	if !ok {
		return 1
	}
	return 0
}

// refreshServed re-lists every pinned Databricks profile's endpoints into the served-models cache.
func refreshServed() {
	for _, err := range preflight.RefreshPinned(context.Background()) {
		fmt.Fprintf(os.Stderr, "doctor --refresh: %v\n", err)
	}
}

// discoverProfile re-seals one profile's snapshot by its registry row: github or Databricks.
func discoverProfile(ctx context.Context, name string) int {
	reg, err := providers.Load()
	if err != nil {
		return fail("preflight: %v", err)
	}
	p := reg.Providers[name]
	switch {
	case p != nil && p.Provider == "github":
		_, err = copilot.RefreshCatalog(ctx)
	case p != nil && p.Preset == providers.PresetDatabricks:
		err = copilot.RefreshCopilotModels(ctx, name)
	default:
		return fail("preflight: %q is not a Databricks or github profile", name)
	}
	if err != nil {
		return fail("preflight: %v", err)
	}
	return 0
}

// cmdPreflight is the hidden `preflight [--live|--refresh]`: pin rows, the post-install step, or a silent re-list.
func cmdPreflight(args []string) int {
	fs := helptext.NewFlagSet("preflight", helptext.Page{
		Usage: "claude-code preflight [--live|--refresh|--discover <profile>]",
		About: "Print each agent's pin check; --live also refreshes stale listings and runs the post-install pong round.\n" +
			"--refresh only re-lists stale listings, silently.\n" +
			"--discover re-seals one profile's model snapshot (Databricks or github), silently (a launch spawns it when expired).",
	})
	live := fs.Bool("live", false, "refresh stale listings and ask each pinned agent for pong")
	refresh := fs.Bool("refresh", false, "re-list stale served-endpoint caches, print nothing")
	discover := fs.String("discover", "", "re-seal one profile's model snapshot (Databricks or github)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *discover != "" {
		ctx, cancel := context.WithTimeout(context.Background(), preflight.Budget)
		defer cancel()
		return discoverProfile(ctx, *discover)
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("preflight: %v", err)
	}
	if *refresh {
		now := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), preflight.Budget)
		defer cancel()
		preflight.RefreshStale(ctx, reg, preflight.Check(reg, now), now, false)
		return 0
	}
	if *live {
		bin, err := os.Executable()
		if err != nil {
			return fail("preflight: %v", err)
		}
		preflight.RunInstallStep(context.Background(), os.Stdout, reg, bin, time.Now())
		return 0
	}
	for _, p := range preflight.Check(reg, time.Now()) {
		fmt.Printf("%-8s %-9s %s\n", p.Agent, p.Status, p.Line())
	}
	return 0
}

// printAudit prints the typed action log: switches, pins, secret sets, safety blocks, proxy
// failovers. Shared by `doctor --audit`.
func printAudit(days int, format string) int {
	if days <= 0 {
		helptext.Fprint(os.Stderr, "usage: claude-code doctor --audit [--days 7] [--format text|json]\n")
		return 2
	}
	ev, err := audit.Read(time.Now().AddDate(0, 0, -days))
	if err != nil {
		return fail("doctor --audit: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Days    int                  `json:"days"`
			Summary map[audit.Action]int `json:"summary"`
			Events  []audit.Event        `json:"events"`
		}{days, audit.Summary(ev), ev}); err != nil {
			return fail("doctor --audit: %v", err)
		}
		return 0
	}
	if len(ev) == 0 {
		fmt.Printf("no audited actions in the last %d day(s) (%s)\n", days, audit.File())
		return 0
	}
	for _, e := range ev {
		fmt.Printf("%s  %-16s %-22s %s\n", e.Time.Local().Format("2006-01-02 15:04"), e.Action, e.Resource, e.Detail)
	}
	return 0
}
