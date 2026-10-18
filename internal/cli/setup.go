// setup is one verb for the three setup modes: adopt an existing ~/.claude, nuke it for a fresh
// start, or scope a project's own .claude/ (workspace dispatches to setup_workspace.go).
package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/setup"
)

const setupUsage = `usage: claude-code setup adopt --from DIR [--into DIR] [--dry-run] [--label S]
       claude-code setup nuke [--dir DIR] [--apply] [--keep-memory] [--account] [--cli]
       claude-code setup workspace init|link|status ...`

func cmdSetup(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, setupUsage)
		return 2
	}
	switch args[0] {
	case "adopt":
		return cmdSetupAdopt(args[1:])
	case "nuke":
		return cmdSetupNuke(args[1:])
	case "workspace":
		return cmdSetupWorkspace(args[1:])
	}
	return fail("setup: unknown mode %q (adopt|nuke|workspace)", args[0])
}

func cmdSetupAdopt(args []string) int {
	fs := flag.NewFlagSet("setup adopt", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	from := fs.String("from", "", "the staged clone to adopt from")
	into := fs.String("into", paths.ConfigDir(), "the existing ~/.claude to adopt into")
	dryRun := fs.Bool("dry-run", false, "report clashes and stop")
	label := fs.String("label", "adopt", "backup directory label")
	if fs.Parse(args) != nil {
		return 2
	}
	if *from == "" {
		fmt.Fprintln(os.Stderr, "claude-code: setup adopt: --from is required")
		return 2
	}
	err := setup.Adopt(setup.AdoptOpts{From: *from, Into: *into, Label: *label, DryRun: *dryRun, Out: os.Stdout})
	return setupExit(err, "setup adopt")
}

func cmdSetupNuke(args []string) int {
	fs := flag.NewFlagSet("setup nuke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", paths.ConfigDir(), "the harness directory to remove")
	apply := fs.Bool("apply", false, "actually remove it (dry-run without this flag)")
	keepMemory := fs.Bool("keep-memory", false, "also archive memory/ (the local memory Git store, incl. .git), docs/memory/bank, plans/ and projects/*/memory")
	account := fs.Bool("account", false, "report what removing the Claude Code account state would take")
	removeCLI := fs.Bool("cli", false, "report what removing the Claude Code CLI install would take")
	if fs.Parse(args) != nil {
		return 2
	}
	if *account {
		fmt.Printf("setup nuke --account is informational only; would remove %s\nreinstall: log in again with `claude-code configure`\n", paths.ClaudeJSON())
	}
	if *removeCLI {
		fmt.Println("setup nuke --cli is informational only; would remove ~/.local/bin/claude")
		fmt.Println("reinstall: https://docs.claude.com/en/docs/claude-code/quickstart")
	}
	confirm := ""
	if *apply {
		fmt.Print("type the directory path to confirm: ")
		fmt.Scanln(&confirm)
	}
	err := setup.Nuke(setup.NukeOpts{Dir: *dir, Apply: *apply, Confirm: confirm, KeepMemory: *keepMemory, Out: os.Stdout})
	return setupExit(err, "setup nuke")
}

// setupExit maps a refusal (bad precondition, wrong confirmation) to exit 2, any other error to 1.
func setupExit(err error, verb string) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, setup.ErrAlreadyCheckout), errors.Is(err, setup.ErrNotAClone), errors.Is(err, setup.ErrConfirmMismatch), errors.Is(err, setup.ErrNotConfigDir):
		fmt.Fprintf(os.Stderr, "claude-code: %s: %v\n", verb, err)
		return 2
	default:
		return fail("%s: %v", verb, err)
	}
}
