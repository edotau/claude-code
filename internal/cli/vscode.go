package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vscode"
)

// cmdVSCode merges the tracked VS Code settings template into VS Code's Machine settings.json.
// Machine scope is per-box and is NOT Settings-Synced, so this is what seeds a fresh box instead.
func cmdVSCode(args []string) int {
	fs := flag.NewFlagSet("vscode", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: claude-code vscode [--check] [--dry-run] [--dest <settings.json>] [--template <file>]")
		fmt.Fprintln(os.Stderr, "                           [--keybindings-dest <file>] [--keybindings-template <file>]")
		fmt.Fprintln(os.Stderr, "                           [--extensions-dir <dir>]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Merges the embedded first-party settings template into VS Code's MACHINE settings —")
		fmt.Fprintln(os.Stderr, "that scope is not Settings-Synced, so each box renders its own copy from this template.")
		fmt.Fprintln(os.Stderr, "Also merges the embedded keybindings template into the local client's keybindings.json")
		fmt.Fprintln(os.Stderr, "(keybindings are UI-side, so a headless box with only ~/.vscode-server skips them).")
		fmt.Fprintln(os.Stderr, "Also symlinks the Claude Code Agents extension into the detected extensions dir.")
		fmt.Fprintln(os.Stderr, "")
		fs.PrintDefaults()
	}
	template := fs.String("template", "", "settings template to merge (default: the one embedded in the binary)")
	dest := fs.String("dest", "", "VS Code Machine settings.json (default: platform-detected)")
	extensionsDir := fs.String("extensions-dir", "", "VS Code extensions dir to link the Agents extension into (default: platform-detected)")
	keybindingsTemplate := fs.String("keybindings-template", "", "keybindings template to merge (default: the one embedded in the binary)")
	keybindingsDest := fs.String("keybindings-dest", "", "VS Code User keybindings.json (default: platform-detected)")
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

	kbDestPath, skip, note := *keybindingsDest, false, ""
	switch {
	case kbDestPath != "":
		// explicit --keybindings-dest wins outright.
	case *dest != "":
		// Explicit --dest with no --keybindings-dest: same directory as the settings file the
		// caller chose, never the platform's vscode-server Machine guess for an unrelated dest.
		kbDestPath = filepath.Join(filepath.Dir(destPath), "keybindings.json")
	default:
		d, skipIt, n, err := vscode.DefaultKeybindingsDest()
		if err != nil {
			return fail("vscode: %v", err)
		}
		kbDestPath, skip, note = d, skipIt, n
	}
	var kbResult vscode.Result
	if !skip {
		kbResult, err = vscode.SyncKeybindings(*keybindingsTemplate, kbDestPath, *check || *dryRun)
		if err != nil {
			return fail("vscode: %v", err)
		}
	}

	extDirPath := *extensionsDir
	if extDirPath == "" {
		d, err := vscode.DefaultExtensionsDir()
		if err != nil {
			return fail("vscode: %v", err)
		}
		extDirPath = d
	}
	extResult, err := vscode.LinkExtension(paths.ConfigDir(), extDirPath, *check || *dryRun)
	if err != nil {
		return fail("vscode: %v", err)
	}

	if *check {
		drift := false
		if result.Changed {
			fmt.Fprintf(os.Stderr, "claude-code: vscode DRIFT %s\n", destPath)
			drift = true
		}
		if !skip && kbResult.Changed {
			fmt.Fprintf(os.Stderr, "claude-code: vscode DRIFT %s\n", kbDestPath)
			drift = true
		}
		if extResult.Changed {
			fmt.Fprintf(os.Stderr, "claude-code: vscode DRIFT %s\n", extResult.Target)
			drift = true
		}
		if drift {
			return 1
		}
		reportMerge("template", destPath, result)
		if skip {
			fmt.Printf("claude-code: vscode keybindings skipped: %s\n", note)
		} else {
			reportMerge("keybindings template", kbDestPath, kbResult)
		}
		fmt.Printf("claude-code: vscode extension in sync %s\n", extResult.Target)
		return 0
	}
	if *dryRun {
		reportDryRun("template", destPath, result.Changed)
		if skip {
			fmt.Printf("claude-code: vscode keybindings skipped: %s\n", note)
		} else {
			reportDryRun("keybindings template", kbDestPath, kbResult.Changed)
		}
		reportDryRun("extension", extResult.Target, extResult.Changed)
		return 0
	}
	reportMerge("template", destPath, result)
	if skip {
		fmt.Printf("claude-code: vscode keybindings skipped: %s\n", note)
	} else {
		reportMerge("keybindings template", kbDestPath, kbResult)
	}
	if extResult.Changed {
		fmt.Printf("claude-code: vscode linked extension -> %s\n", extResult.Target)
	} else {
		fmt.Printf("claude-code: vscode extension in sync %s\n", extResult.Target)
	}
	return 0
}

func reportDryRun(label, dest string, changed bool) {
	if changed {
		fmt.Printf("claude-code: vscode would merge %s -> %s\n", label, dest)
	} else {
		fmt.Printf("claude-code: vscode %s in sync %s\n", label, dest)
	}
}

func reportMerge(label, dest string, result vscode.Result) {
	switch {
	case !result.Changed:
		fmt.Printf("claude-code: vscode %s in sync %s\n", label, dest)
	case result.Backup == "":
		fmt.Printf("claude-code: vscode merged %s -> %s\n", label, dest)
	default:
		fmt.Printf("claude-code: vscode merged %s -> %s (backup %s)\n", label, dest, result.Backup)
	}
}
