package cli

import (
	"context"
	"flag"
	"fmt"
	"github.com/edotau/claude-code/internal/history"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/statusline"
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
		fmt.Fprintln(os.Stderr, "usage: claude-code run <codex|gemini|opencode|copilot> [--provider p]\n"+
			"       claude-code run [--model [provider:]model] [--print-env] [args...]")
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
	if len(args) == 0 {
		return fail("usage: claude-code hook <safety|stop-format|context-checkpoint|session-start|\n" +
			"        memory-recall|session-harvest|session-harvest-end> < payload.json")
	}
	return hooks.Run(args[0], os.Stdin, os.Stdout, os.Stderr)
}

func cmdStatusline(args []string) int { return statusline.Run(args, os.Stdin, os.Stdout) }

func cmdSettings(args []string) int {
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	out := fs.String("out", "", "write the render here (atomically, after a timestamped backup)")
	agents := fs.String("agents", "", "comma-separated vendors to render (default codex,opencode,gemini,copilot)")
	dryRun := fs.Bool("dry-run", false, "print every rendered block (settings.json and vendor configs); write nothing")
	check := fs.Bool("check", false, "exit 1 if any rendered file has drifted; write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	data, err := settings.Render()
	if err != nil {
		return fail("%v", err)
	}
	if *out != "" && !*dryRun && !*check {
		if r, err := history.SnapshotConfig("pre-generate"); err != nil {
			fmt.Fprintln(os.Stderr, "generate: history snapshot:", err)
		} else if r.Commit != "" {
			fmt.Printf("history %s (pre-generate snapshot)\n", r.Commit)
		}
	}
	switch {
	case *out != "" && *check: // drift report only; the vendor pass below decides the exit code
	case *out == "" || *dryRun: // a preview: --out names where it WOULD go, never a write
		if !*check {
			os.Stdout.Write(data)
		}
	default:
		backup, err := settings.WriteWithBackup(*out, data)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Printf("wrote %s", *out)
		if backup != "" {
			fmt.Printf(" (backup %s)", backup)
		}
		fmt.Println()
	}

	var vendorAgents []string
	if *agents != "" {
		vendorAgents = strings.Split(*agents, ",")
	}
	// Without --out the settings render is a preview, so the vendor configs preview too: nothing is written.
	preview := *dryRun || (*out == "" && !*check)
	drift, err := generate.Run(generate.Options{Agents: vendorAgents, DryRun: preview, Check: *check, Out: os.Stdout})
	if err != nil {
		return fail("%v", err)
	}
	if *check && drift {
		return 1
	}
	return 0
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "print what would change; write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := launch.Install(os.Stdout, *dry); err != nil {
		return fail("install: %v", err)
	}
	return 0
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
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
	ok := true
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
		fmt.Fprintln(os.Stderr, "usage: claude-code doctor --audit [--days 7] [--format text|json]")
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
