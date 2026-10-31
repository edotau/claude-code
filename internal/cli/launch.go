package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
	usagecmd "github.com/edotau/claude-code/internal/usage"
)

func cmdClaude(args []string) int { return launchClaude("", args) }

// launchClaude launches Claude Code with tier as the main-loop model ("" = opus).
func launchClaude(tier string, args []string) int {
	opts, rest, err := launch.ParseArgs(args, true)
	if err != nil {
		return fail("%v", err)
	}
	opts.Tier = tier
	plan, err := launch.Claude(context.Background(), opts, rest)
	return runPlan(plan, opts, err)
}

// tierVerb is `claude-code <alias>`: Claude Code on the session profile with that alias as the main model.
func tierVerb(tier string) func([]string) int {
	return func(a []string) int { return launchClaude(tier, a) }
}

var anthropicPage = helptext.Page{
	Usage: "claude-code anthropic [--oauth[=NAME]] [--x-api-key[=NAME]] [--model M]\n" +
		"[--print-env] [--keep-env NAMES] [claude args]",
	About: "Claude Code on first-party Anthropic. No flag uses your claude.ai login. A bare flag takes no " +
		"value; =NAME picks the vault key.",
	Hint: "claude-code anthropic " + helptext.DocsFlag + " prints claude's own help.",
}

// anthropicHelp renders the page with its flag rows; the flags are parsed by launch.ParseAnthropicArgs.
func anthropicHelp(w io.Writer) {
	fs := helptext.NewFlagSet("anthropic", anthropicPage)
	fs.Bool("oauth", false, "setup-token login; =NAME picks the vault key")
	fs.Bool("x-api-key", false, "API key login; =NAME picks the vault key")
	fs.String("model", "", "alias (opus), version (opus-5-5) or wire id: the main model")
	fs.Bool("print-env", false, "print the resolved launch; never prompts")
	fs.String("keep-env", "", "env names the credential scrub keeps")
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
	o, rest, err := launch.ParseAnthropicArgs(args)
	if err != nil {
		return fail("%v", err)
	}
	secret := ""
	if o.Mode != launch.AuthLogin {
		if secret, err = anthropicSecret(o); err != nil {
			return fail("%v", err)
		}
	}
	plan, err := launch.Anthropic(context.Background(), o, rest, secret)
	return runPlan(plan, o.Options, err)
}

// anthropicSecret reads the mode's vault key; when it is missing and a terminal is attached, offers to store it.
func anthropicSecret(o launch.AnthropicOptions) (string, error) {
	read := func() (string, error) {
		if o.Mode == launch.AuthOAuth {
			return providers.StoredSecret(o.Name), nil
		}
		v, _, err := providers.EffectiveSecret(o.Name)
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
	if o.Mode == launch.AuthOAuth && tui.Confirm("Run `claude setup-token` first to mint one?", true) {
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
	bin, err := agent.ClaudeBinary()
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

func launchVendor(name string, args []string) int {
	opts, rest, err := launch.ParseArgs(args, false)
	if err != nil {
		return fail("%v", err)
	}
	plan, err := launch.Vendor(context.Background(), name, opts, rest)
	return runPlan(plan, opts, err)
}

func cmdUsage(args []string) int { return usagecmd.Run(args, paths.ConfigDir(), os.Stdout, os.Stderr) }

// cmdScience takes only a leading --print-env; every other flag is claude-science's own.
func cmdScience(args []string) int {
	var opts launch.Options
	if len(args) > 0 && args[0] == "--print-env" {
		opts.PrintEnv, args = true, args[1:]
	}
	plan, err := launch.ScienceEnv(args)
	return runPlan(plan, opts, err)
}

func runPlan(plan launch.Plan, opts launch.Options, err error) int {
	if usage, ok := errors.AsType[launch.UsageError](err); ok {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if err != nil {
		return fail("%v", err)
	}
	if opts.PrintEnv {
		plan.Print(os.Stdout)
		return 0
	}
	return fail("exec %s: %v", plan.Binary, plan.Exec())
}

// hookPage lists modules in Hint, not Commands: a COMMANDS row would invite `hook <name> -h`, which runs the hook.
var hookPage = helptext.Page{
	Usage: "claude-code hook <Event> [--<module>|--all] < payload.json",
	About: "Run one lifecycle hook on the event JSON on stdin; settings.json and the agent hook files invoke it.",
	Hint: "modules: safety-scan|rtk-rewrite|docs-placement|dispatch-effort|\n" +
		"         dispatch-failure|dispatch-workflow|dispatch-stop|\n" +
		"         docs-plan-mirror|docs-plan-save|memory-update|format-stop|\n" +
		"         memory-start|paths-resolve|memory-recall|review-simplicity|\n" +
		"         memory-end|memory-compact|worktree-create|worktree-remove\n" +
		"events: <Event> runs the dispatcher, --<module> one module, --all every module",
}

func cmdHook(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		hookPage.Fprint(os.Stderr)
		return 0
	}
	return hooks.RunArgs(args[0], args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// cmdRestore is `configure --restore`: write settings.json whole from the defaults, then render the agent configs.
func cmdRestore(agents, out string, check, dryRun, vendors, noAgents bool) int {
	if out == "" && !check && !dryRun { // bare --restore writes the live settings.json
		out = launch.SettingsFile()
	}
	data, err := settings.RenderLive()
	if err != nil {
		return fail("%v", err)
	}
	var prepared *settings.Prepared
	var drift bool
	if !noAgents { // --no-agents: lint checks the repo's settings, not this machine's agent configs
		if prepared, drift, err = renderVendorConfigs(agents, out, check, dryRun, vendors); err != nil {
			return fail("%v", err)
		}
	}
	var outDrift bool
	if out != "" && !check && !dryRun {
		err = restoreSettingsFile(out)
	} else {
		outDrift, err = writeSettingsOutput(out, check, dryRun, data)
	}
	if err != nil {
		return fail("%v", err)
	}
	winDrift, err := writeWindowsSettingsOutput(out, check)
	if err != nil {
		return fail("%v", err)
	}
	if prepared != nil {
		if err := prepared.Apply(); err != nil {
			return fail("%v", err)
		}
	}
	if check && (drift || outDrift || winDrift) {
		return 1
	}
	return 0
}

// writeWindowsSettingsOutput reports Windows settings.json drift under --check; the Windows file path
// is fixed (not overridable via --out), and only `install` writes it, so bare/--dry-run generate stay
// read-only here too, matching settings.json's own preview-only default.
func writeWindowsSettingsOutput(out string, check bool) (bool, error) {
	if out != "" || !check {
		return false, nil
	}
	return checkLiveWindowsSettings()
}

// checkLiveWindowsSettings prints the Windows drift findings and reports whether it has drifted;
// ok=false (no Windows profile resolved) prints nothing and is not drift.
func checkLiveWindowsSettings() (bool, error) {
	drift, ok, err := launch.CheckWindowsSettingsDrift()
	if err != nil || !ok {
		return false, err
	}
	for _, l := range drift.LinesLabeled("settings (windows)") {
		fmt.Println(l)
	}
	return drift.Drifted(), nil
}

// restoreSettingsFile snapshots history, then writes the whole render to file and reports both.
func restoreSettingsFile(file string) error {
	r, err := launch.RestoreSettings(file)
	if r.Commit != "" {
		fmt.Printf("history %s (before restore snapshot)\n", r.Commit)
	}
	if err != nil {
		return err
	}
	fmt.Printf("restored %s", file)
	if r.Backup != "" {
		fmt.Printf(" (backup %s)", r.Backup)
	}
	fmt.Println()
	return nil
}

// writeSettingsOutput writes the rendered settings per --out/--check/--dry-run and reports drift.
func writeSettingsOutput(out string, check, dryRun bool, data []byte) (bool, error) {
	switch {
	case out != "" && check: // drift report only: the rendered settings against the --out file
		if cur, err := os.ReadFile(out); err != nil || !bytes.Equal(cur, data) {
			fmt.Printf("settings: %s has drifted from the render\n", out)
			return true, nil
		}
		return false, nil
	case out == "" && check: // no --out: check the live settings.json, settings.local.json and nested config
		return checkLiveSettings(data)
	case out == "" || dryRun: // a preview: --out names where it WOULD go, never a write
		_, err := os.Stdout.Write(data)
		return false, err
	default:
		backup, err := settings.WriteWithBackup(out, data)
		if err != nil {
			return false, err
		}
		fmt.Printf("wrote %s", out)
		if backup != "" {
			fmt.Printf(" (backup %s)", backup)
		}
		fmt.Println()
		return false, nil
	}
}

// checkLiveSettings prints the drift findings for settings.json and reports whether it has drifted.
func checkLiveSettings(rendered []byte) (bool, error) {
	drift, err := launch.CheckSettingsDriftWith(rendered)
	if err != nil {
		return false, err
	}
	for _, l := range drift.Lines() {
		fmt.Println(l)
	}
	return drift.Drifted(), nil
}

// renderVendorConfigs prepares the vendor configs; they preview whenever the settings render does.
func renderVendorConfigs(agents, out string, check, dryRun, vendors bool) (*settings.Prepared, bool, error) {
	var vendorAgents []string
	if agents != "" {
		vendorAgents = strings.Split(agents, ",")
	}
	preview := dryRun || (out == "" && !check)
	opts := settings.Options{Agents: vendorAgents, DryRun: preview, Check: check, Out: os.Stdout, Vendors: vendors, Router: agent.Router{}}
	// Interactive only on a real terminal write: launch/configure callers never reach this function.
	if !preview && !check && tui.StdinIsTerminal() && tui.StdoutIsTerminal() {
		opts.Confirm = func(path string, lost []string) bool {
			return tui.ConfirmKey(fmt.Sprintf("overwrite %s?", path), true)
		}
	}
	return settings.Prepare(opts)
}

// cmdRevert handles `configure --revert [--agent a,b] [--dry-run]`.
func cmdRevert(agents string, dryRun bool) int {
	var vendorAgents []string
	if agents != "" {
		vendorAgents = strings.Split(agents, ",")
	}
	if err := settings.Revert(settings.Options{Agents: vendorAgents, DryRun: dryRun, Out: os.Stdout, Router: agent.Router{}}); err != nil {
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
		if err := agent.InstallSelected(context.Background(), os.Stdout, tf.names, *dry); err != nil {
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
		return secretAudits(*scan || *all, *gitScan || *all, *all, fs.Args(), *rev, *since)
	}
	if *auditMode {
		return printAudit(*days, *format)
	}
	if *refresh {
		refreshServed()
	}
	var ok bool
	if *format == helptext.JSON {
		var checks []launch.Check
		checks, ok = doctorChecks()
		if err := helptext.WriteJSON(os.Stdout, struct {
			OK     bool           `json:"ok"`
			Checks []launch.Check `json:"checks"`
		}{ok, checks}); err != nil {
			return fail("doctor: %v", err)
		}
	} else {
		ok = launch.Doctor(os.Stdout)
	}
	if !ok {
		return 1
	}
	return 0
}

// refreshServed re-lists every pinned Databricks profile's endpoints into the served-models cache.
func refreshServed() {
	reg, err := providers.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor --refresh: %v\n", err)
		return
	}
	ps := preflight.Check(reg, time.Now())
	for _, err := range preflight.RefreshStale(context.Background(), reg, ps, time.Now(), true) {
		fmt.Fprintf(os.Stderr, "doctor --refresh: %v\n", err)
	}
}

// cmdPreflight is the hidden `preflight [--live|--refresh]`: pin rows, the post-install step, or a silent re-list.
func cmdPreflight(args []string) int {
	fs := helptext.NewFlagSet("preflight", helptext.Page{
		Usage: "claude-code preflight [--live|--refresh]",
		About: "Print each agent's pin check; --live also refreshes stale listings and runs the post-install pong round.\n" +
			"--refresh only re-lists stale listings, silently.",
	})
	live := fs.Bool("live", false, "refresh stale listings and ask each pinned agent for pong")
	refresh := fs.Bool("refresh", false, "re-list stale served-endpoint caches, print nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
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
