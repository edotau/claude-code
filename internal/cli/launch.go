package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/history"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/statusline"
	"github.com/edotau/claude-code/internal/tools"
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

func cmdRun(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		helptext.Fprint(os.Stderr, "usage: claude-code run <codex|gemini|opencode|copilot> [--provider p]\n"+
			"       claude-code run [--model [provider:]model] [--print-env]\n"+
			"                       [--keep-env NAME[,NAME]] [args...]\n")
		return 2
	}
	return launchVendor(args[0], args[1:])
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

func cmdHook(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		helptext.Fprint(os.Stderr, "usage: claude-code hook <name> < payload.json\n"+
			"  names: safety|stop-format|context-checkpoint|session-start|\n"+
			"         memory-recall|session-harvest|session-harvest-end\n")
		return 1
	}
	return hooks.Run(args[0], os.Stdin, os.Stdout, os.Stderr)
}

func cmdStatusline(args []string) int { return statusline.Run(args, os.Stdin, os.Stdout) }

func cmdSettings(args []string) int {
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	out := fs.String("out", "", "write the render here (atomically, after a timestamped backup)")
	agents := fs.String("agents", "", "vendors to render (default: codex,opencode,gemini,copilot)")
	dryRun := fs.Bool("dry-run", false, "preview settings and vendor configs; write nothing")
	check := fs.Bool("check", false, "exit 1 if settings.json or a vendor config drifted")
	vendors := fs.Bool("vendors", false, "render vendor configs even when CLAUDE_CONFIG_DIR is not the default ~/.claude")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	data, err := settings.RenderLive()
	if err != nil {
		return fail("%v", err)
	}
	prepared, drift, err := renderVendorConfigs(*agents, *out, *check, *dryRun, *vendors)
	if err != nil {
		return fail("%v", err)
	}
	snapshotSettingsHistory(*out, *dryRun, *check)
	outDrift, err := writeSettingsOutput(*out, *check, *dryRun, data)
	if err != nil {
		return fail("%v", err)
	}
	winDrift, err := writeWindowsSettingsOutput(*out, *check, *dryRun)
	if err != nil {
		return fail("%v", err)
	}
	if err := prepared.Apply(); err != nil {
		return fail("%v", err)
	}
	if *check && (drift || outDrift || winDrift) {
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

// renderVendorConfigs prepares the vendor configs; without --out the settings render is a preview,
// so the vendor configs preview too: nothing is written.
func renderVendorConfigs(agents, out string, check, dryRun, vendors bool) (*generate.Prepared, bool, error) {
	var vendorAgents []string
	if agents != "" {
		vendorAgents = strings.Split(agents, ",")
	}
	preview := dryRun || (out == "" && !check)
	return generate.Prepare(generate.Options{Agents: vendorAgents, DryRun: preview, Check: check, Out: os.Stdout, Vendors: vendors})
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
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	dry := fs.Bool("dry-run", false, "print what would change; write nothing")
	var tf toolsFlag
	fs.Var(&tf, "tools", "install CLIs only: bare = all; --tools=name,... selects tools")
	setup := fs.String("setup", "", "adopt|nuke|workspace: migrate, wipe, or scope .claude/ (first arg)")
	if err := fs.Parse(args); err != nil {
		return 2
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
	if err := launch.Install(os.Stdout, *dry); err != nil {
		return fail("install: %v", err)
	}
	return 0
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	format := outfmt.Flag(fs)
	auditMode := fs.Bool("audit", false, "print the typed action log instead of health checks")
	days := fs.Int("days", 7, "with --audit: events from the last N days")
	if fs.Parse(args) != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fail("doctor: %v", err)
		return 2
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
