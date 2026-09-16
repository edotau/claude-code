package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/statusline"
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
		fmt.Fprintln(os.Stderr, "usage: claude-code run <codex|gemini|opencode|copilot> [--provider p] [--model [provider:]model] [--print-env] [args...]")
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
		return fail("usage: claude-code hook <safety|stop-format|context-checkpoint|session-start|memory-recall|session-harvest|session-harvest-end> < payload.json")
	}
	return hooks.Run(args[0], os.Stdin, os.Stdout, os.Stderr)
}

func cmdStatusline(args []string) int { return statusline.Run(args, os.Stdin, os.Stdout) }

func cmdSettings(args []string) int {
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	out := fs.String("out", "", "write the render here (atomically, after a timestamped backup)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	data, err := settings.Render()
	if err != nil {
		return fail("%v", err)
	}
	if *out == "" {
		os.Stdout.Write(data)
		return 0
	}
	backup, err := settings.WriteWithBackup(*out, data)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("wrote %s", *out)
	if backup != "" {
		fmt.Printf(" (backup %s)", backup)
	}
	fmt.Println()
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

func cmdDoctor([]string) int {
	if !launch.Doctor(os.Stdout) {
		return 1
	}
	return 0
}
