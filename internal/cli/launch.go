package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/statusline"
	usagecmd "github.com/edotau/claude-code/internal/usage"
)

func cmdClaude(args []string) int {
	opts, rest, err := launch.ParseArgs(args, true)
	if err != nil {
		return fail("%v", err)
	}
	plan, err := launch.Claude(context.Background(), opts, rest)
	return runPlan(plan, opts, err)
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
	if *out == "" {
		if !*check { // --check reports drift only; the render itself is noise there
			os.Stdout.Write(data)
		}
	} else {
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
	if fs.Parse(args) != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fail("doctor: %v", err)
		return 2
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
