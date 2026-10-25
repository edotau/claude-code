package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/history"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/tools"
	"github.com/edotau/claude-code/internal/tui"
	usagecmd "github.com/edotau/claude-code/internal/usage"
)

func cmdClaude(args []string) int { return launchClaude("", args) }

// launchClaude launches Claude Code with tier as the main-loop model ("" = the opus slot).
func launchClaude(tier string, args []string) int {
	opts, rest, err := launch.ParseArgs(args, true)
	if err != nil {
		return fail("%v", err)
	}
	opts.Tier = tier
	plan, err := launch.Claude(context.Background(), opts, rest)
	return runPlan(plan, opts, err)
}

// tierVerb is `claude-code <tier>`: Claude Code on the session provider with that tier as the main model.
func tierVerb(tier string) func([]string) int {
	return func(a []string) int { return launchClaude(tier, a) }
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
	if err != nil {
		return fail("%v", err)
	}
	if opts.PrintEnv {
		plan.Print(os.Stdout)
		return 0
	}
	return fail("exec %s: %v", plan.Binary, plan.Exec())
}

// hookPage lists names in Hint, not Commands: a COMMANDS row would invite `hook <name> -h`, which runs the hook.
var hookPage = helptext.Page{
	Usage: "claude-code hook <name> < payload.json",
	About: "Run one lifecycle hook on the event JSON on stdin; settings.json and the vendor hook files invoke it.",
	Hint: "names: safety|stop-format|context-checkpoint|session-start|\n" +
		"       memory-recall|session-harvest|session-harvest-end",
}

func cmdHook(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		hookPage.Fprint(os.Stderr)
		return 0
	}
	return hooks.Run(args[0], os.Stdin, os.Stdout, os.Stderr)
}

// cmdReset is `configure --reset`: render settings.json and the vendor configs from the templates.
func cmdReset(agents, out string, check, dryRun, vendors bool) int {
	if out == "" && !check && !dryRun { // bare --reset writes the live settings.json
		out = launch.SettingsFile()
	}
	data, err := settings.RenderLive()
	if err != nil {
		return fail("%v", err)
	}
	prepared, drift, err := renderVendorConfigs(agents, out, check, dryRun, vendors)
	if err != nil {
		return fail("%v", err)
	}
	snapshotSettingsHistory(out, dryRun, check)
	outDrift, err := writeSettingsOutput(out, check, dryRun, data)
	if err != nil {
		return fail("%v", err)
	}
	winDrift, err := writeWindowsSettingsOutput(out, check, dryRun)
	if err != nil {
		return fail("%v", err)
	}
	if err := prepared.Apply(); err != nil {
		return fail("%v", err)
	}
	if check && (drift || outDrift || winDrift) {
		return 1
	}
	return 0
}

// writeWindowsSettingsOutput reports Windows settings.json drift under --check; the Windows file path
// is fixed (not overridable via --out), and only `install` writes it, so bare/--dry-run generate stay
// read-only here too, matching settings.json's own preview-only default.
func writeWindowsSettingsOutput(out string, check, _ bool) (bool, error) {
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

func snapshotSettingsHistory(out string, dryRun, check bool) {
	if out == "" || dryRun || check {
		return
	}
	if r, err := history.SnapshotConfig("pre-generate"); err != nil {
		fmt.Fprintln(os.Stderr, "generate: history snapshot:", err)
	} else if r.Commit != "" {
		fmt.Printf("history %s (pre-generate snapshot)\n", r.Commit)
	}
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
func renderVendorConfigs(agents, out string, check, dryRun, vendors bool) (*generate.Prepared, bool, error) {
	var vendorAgents []string
	if agents != "" {
		vendorAgents = strings.Split(agents, ",")
	}
	preview := dryRun || (out == "" && !check)
	opts := generate.Options{Agents: vendorAgents, DryRun: preview, Check: check, Out: os.Stdout, Vendors: vendors}
	// Interactive only on a real terminal write: launch/configure callers never reach this function.
	if !preview && !check && tui.StdinIsTerminal() && tui.StdoutIsTerminal() {
		opts.Confirm = func(path string, lost []string) bool {
			return tui.ConfirmKey(fmt.Sprintf("overwrite %s?", path), true)
		}
	}
	return generate.Prepare(opts)
}

// cmdRestore handles `configure --restore [--agent a,b] [--dry-run]`.
func cmdRestore(agents string, dryRun bool) int {
	var vendorAgents []string
	if agents != "" {
		vendorAgents = strings.Split(agents, ",")
	}
	if err := generate.Revert(generate.Options{Agents: vendorAgents, DryRun: dryRun, Out: os.Stdout}); err != nil {
		return fail("configure --restore: %v", err)
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
		About: "Build-time setup: install the Claude Code CLI and vendor links, then render settings.json. " +
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
		if err := tools.InstallSelected(context.Background(), os.Stdout, tf.names, *dry); err != nil {
			return fail("install --tools: %v", err)
		}
		return 0
	}
	if err := launch.Install(os.Stdout, *dry, *verbose); err != nil {
		return fail("install: %v", err)
	}
	if err := configure.MigrateWebSearchArgs(*dry, os.Stdout); err != nil {
		return fail("install: web-search MCP migrate: %v", err)
	}
	if err := configure.MigrateVendorOverlay(*dry, os.Stdout); err != nil {
		return fail("install: vendor overlay migrate: %v", err)
	}
	return 0
}

func cmdDoctor(args []string) int {
	fs := helptext.NewFlagSet("doctor", helptext.Page{
		Usage: "claude-code doctor [flags] [--scan [path...]]",
		About: "Check binaries, providers, credentials and the router without changing anything; " +
			"exits 1 when a check fails. --audit prints switches, pins, secret sets, safety blocks " +
			"and router failovers instead. --scan, --git and --secrets audit for plaintext " +
			"credentials instead and exit 1 on any finding.",
	})
	format := outfmt.Flag(fs)
	auditMode := fs.Bool("audit", false, "print the typed action log instead of health checks")
	days := fs.Int("days", 7, "with --audit: events from the last N days")
	scan := fs.Bool("scan", false, "scan paths given, else configs and vault, for inline secrets")
	gitScan := fs.Bool("git", false, "scan this repo's git history for committed secrets")
	since := fs.Int("since", 0, "with --git or --secrets: only the last N commits (0 = all)")
	rev := fs.String("range", "", "with --git or --secrets: only this revision range, e.g. @{u}..HEAD")
	all := fs.Bool("secrets", false, "--scan, --git and the vault checks together; worst exit wins")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := outfmt.Check(*format); err != nil {
		fail("doctor: %v", err)
		return 2
	}
	if len(fs.Args()) > 0 && !*scan {
		fail("doctor: unexpected arguments %v (paths go after --scan)", fs.Args())
		return 2
	}
	if *scan || *gitScan || *all {
		if *format == outfmt.JSON {
			fail("doctor: --scan, --git and --secrets print text only")
			return 2
		}
		return secretAudits(*scan || *all, *gitScan || *all, *all, fs.Args(), *rev, *since)
	}
	if *auditMode {
		return printAudit(*days, *format)
	}
	var ok bool
	if *format == outfmt.JSON {
		var checks []launch.Check
		checks, ok = launch.DoctorChecks()
		_ = outfmt.Write(os.Stdout, struct {
			OK     bool           `json:"ok"`
			Checks []launch.Check `json:"checks"`
		}{ok, checks})
	} else {
		ok = launch.Doctor(os.Stdout)
	}
	if !ok {
		return 1
	}
	return 0
}

// printAudit prints the typed action log: switches, pins, secret sets, safety blocks, router
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
	if format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, struct {
			Days    int                  `json:"days"`
			Summary map[audit.Action]int `json:"summary"`
			Events  []audit.Event        `json:"events"`
		}{days, audit.Summary(ev), ev})
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
