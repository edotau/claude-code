package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/network/vscode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdVSCode merges the tracked VS Code settings template into VS Code's Machine settings.json.
// Machine scope is per-box and is NOT Settings-Synced, so the merge is what seeds a fresh box instead.
func cmdVSCode(args []string) int {
	fs := helptext.NewFlagSet("theme vscode", helptext.Page{Usage: "claude-code theme vscode [flags]",
		About: "Merge embedded templates into VS Code's Machine settings and local keybindings.\n" +
			"Machine settings are per-box and excluded from Settings Sync.\n" +
			"Skip keybindings on headless boxes with only ~/.vscode-server.\n" +
			"Link the Claude Code Agents extension into the detected extensions directory."})
	template := fs.String("template", "", "settings template to merge (default: the one embedded in the binary)")
	dest := fs.String("dest", "", "VS Code Machine settings.json (default: platform-detected)")
	extensionsDir := fs.String("extensions-dir", "", "link the Agents extension here (default: platform-detected)")
	keybindingsTemplate := fs.String("keybindings-template", "", "keybindings template to merge (default: the one embedded in the binary)")
	keybindingsDest := fs.String("keybindings-dest", "", "VS Code User keybindings.json (default: platform-detected)")
	check := fs.Bool("check", false, "report drift without writing; exit 1 on drift")
	dryRun := fs.Bool("dry-run", false, "preview the merge; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 { // a stray word (the retired `serve`) must not fall through to a merge
		return fail("theme vscode: unexpected argument %q (serving moved to `network vscode`)", fs.Arg(0))
	}
	if *check && *dryRun {
		fmt.Fprintln(os.Stderr, "claude-code: theme vscode --check and --dry-run are mutually exclusive")
		return 2
	}
	destPath, err := resolveVSCodeDest(*dest)
	if err != nil {
		return fail("vscode: %v", err)
	}
	result, err := vscode.Sync(*template, destPath, paths.ConfigDir(), paths.BinDir(), *check || *dryRun)
	if err != nil {
		return fail("vscode: %v", err)
	}

	kbDestPath, skip, note, err := resolveKeybindingsDest(*keybindingsDest, *dest, destPath)
	if err != nil {
		return fail("vscode: %v", err)
	}
	var kbResult vscode.Result
	if !skip {
		kbResult, err = vscode.SyncKeybindings(*keybindingsTemplate, kbDestPath, *check || *dryRun)
		if err != nil {
			return fail("vscode: %v", err)
		}
	}

	extDirPath, err := resolveExtensionsDir(*extensionsDir)
	if err != nil {
		return fail("vscode: %v", err)
	}
	extResult, err := vscode.LinkExtension(paths.ConfigDir(), extDirPath, *check || *dryRun)
	if err != nil {
		return fail("vscode: %v", err)
	}

	mode := "apply"
	switch {
	case *check:
		mode = "check"
	case *dryRun:
		mode = "dry-run"
	}
	return vscodeReport(mode, destPath, result, kbDestPath, skip, note, kbResult, extResult)
}

func resolveVSCodeDest(dest string) (string, error) {
	if dest != "" {
		return dest, nil
	}
	return paths.VSCodeSettings()
}

// resolveKeybindingsDest: explicit --keybindings-dest wins outright; explicit --dest with no
// --keybindings-dest uses the same directory as the settings file, never the platform's
// vscode-server Machine guess for an unrelated dest.
func resolveKeybindingsDest(kbDest, dest, destPath string) (path string, skip bool, note string, err error) {
	switch {
	case kbDest != "":
		return kbDest, false, "", nil
	case dest != "":
		return filepath.Join(filepath.Dir(destPath), "keybindings.json"), false, "", nil
	default:
		return vscode.DefaultKeybindingsDest()
	}
}

func resolveExtensionsDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	return vscode.DefaultExtensionsDir()
}

// vscodeReport prints the sync outcome for mode check (exit 1 on drift), dry-run or apply.
func vscodeReport(mode, destPath string, result vscode.Result, kbDestPath string, skip bool, note string, kbResult vscode.Result, extResult vscode.ExtensionResult) int {
	switch mode {
	case "check":
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
		reportKeybindings(skip, note, func() { reportMerge("keybindings template", kbDestPath, kbResult) })
		fmt.Printf("claude-code: vscode extension in sync %s\n", extResult.Target)
	case "dry-run":
		reportDryRun("template", destPath, result.Changed)
		reportKeybindings(skip, note, func() { reportDryRun("keybindings template", kbDestPath, kbResult.Changed) })
		reportDryRun("extension", extResult.Target, extResult.Changed)
	default:
		reportMerge("template", destPath, result)
		reportKeybindings(skip, note, func() { reportMerge("keybindings template", kbDestPath, kbResult) })
		if extResult.Changed {
			fmt.Printf("claude-code: vscode linked extension -> %s\n", extResult.Target)
		} else {
			fmt.Printf("claude-code: vscode extension in sync %s\n", extResult.Target)
		}
	}
	return 0
}

// reportKeybindings prints the skip note, or calls report, depending on whether SyncKeybindings ran.
func reportKeybindings(skip bool, note string, report func()) {
	if skip {
		fmt.Printf("claude-code: vscode keybindings skipped: %s\n", note)
		return
	}
	report()
}

func reportDryRun(label, dest string, changed bool) {
	if changed {
		fmt.Printf("claude-code: vscode would merge %s -> %s\n", label, dest)
	} else {
		fmt.Printf("claude-code: vscode %s in sync %s\n", label, dest)
	}
}
