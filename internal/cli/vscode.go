package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vscode"
)

// cmdVSCode merges the tracked VS Code settings template into VS Code's Machine settings.json.
// Machine scope is per-box and is NOT Settings-Synced, so this is what seeds a fresh box instead.
func cmdVSCode(args []string) int {
	fs := flag.NewFlagSet("vscode", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: claude-code vscode [--check] [--dry-run] [--dest <settings.json>] [--template <file>]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Merges the embedded first-party settings template into VS Code's MACHINE settings —")
		fmt.Fprintln(os.Stderr, "that scope is not Settings-Synced, so each box renders its own copy from this template.")
		fmt.Fprintln(os.Stderr, "")
		fs.PrintDefaults()
	}
	template := fs.String("template", "", "settings template to merge (default: the one embedded in the binary)")
	dest := fs.String("dest", "", "VS Code Machine settings.json (default: platform-detected)")
	check := fs.Bool("check", false, "report drift without writing; exit 1 on drift")
	dryRun := fs.Bool("dry-run", false, "preview the merge; write nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *check && *dryRun {
		fmt.Fprintln(os.Stderr, "claude-code: vscode --check and --dry-run are mutually exclusive")
		return 2
	}
	destPath := *dest
	if destPath == "" {
		d, err := vscode.DefaultDest()
		if err != nil {
			return fail("vscode: %v", err)
		}
		destPath = d
	}
	result, err := vscode.Sync(*template, destPath, paths.ConfigDir(), paths.BinDir(), *check || *dryRun)
	if err != nil {
		return fail("vscode: %v", err)
	}
	if *check && result.Changed {
		fmt.Fprintf(os.Stderr, "claude-code: vscode DRIFT %s\n", destPath)
		return 1
	}
	if *dryRun {
		if result.Changed {
			fmt.Printf("claude-code: vscode would merge template -> %s\n", destPath)
		} else {
			fmt.Printf("claude-code: vscode in sync %s\n", destPath)
		}
		return 0
	}
	switch {
	case !result.Changed:
		fmt.Printf("claude-code: vscode in sync %s\n", destPath)
	case result.Backup == "":
		fmt.Printf("claude-code: vscode merged template -> %s\n", destPath)
	default:
		fmt.Printf("claude-code: vscode merged template -> %s (backup %s)\n", destPath, result.Backup)
	}
	return 0
}
