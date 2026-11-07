package cli

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/network/vscode"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

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

// cmdVSCode merges the tracked VS Code settings template into VS Code's Machine settings.json.
// Machine scope is per-box and is NOT Settings-Synced, so the merge is what seeds a fresh box instead.
func cmdVSCode(args []string) int {
	fs := helptext.NewFlagSet("theme vscode", helptext.Page{Usage: "claude-code theme vscode [flags]",
		About: "Merge embedded templates into VS Code's Machine settings and local keybindings.\n" +
			"Machine settings are per-box and excluded from Settings Sync.\n" +
			"Skip keybindings on headless boxes with only ~/.vscode-server.\n" +
			"Link the Claude Code Agents extension into the detected extensions directory."})
	var o vscode.SyncOptions
	fs.StringVar(&o.Template, "template", "", "settings template to merge (default: the one embedded in the binary)")
	fs.StringVar(&o.Dest, "dest", "", "VS Code Machine settings.json (default: platform-detected)")
	fs.StringVar(&o.ExtensionsDir, "extensions-dir", "", "link the Agents extension here (default: platform-detected)")
	fs.StringVar(&o.KeybindingsTemplate, "keybindings-template", "", "keybindings template to merge (default: the one embedded in the binary)")
	fs.StringVar(&o.KeybindingsDest, "keybindings-dest", "", "VS Code User keybindings.json (default: platform-detected)")
	check := fs.Bool("check", false, "report drift without writing; exit 1 on drift")
	dryRun := fs.Bool("dry-run", false, "preview the merge; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 { // a stray word (the retired `serve`) must not fall through to a merge
		return helptext.Fail("theme vscode: unexpected argument %q (serving moved to `network vscode`)", fs.Arg(0))
	}
	return vscodeSync(o, *check, *dryRun)
}

// vscodeSync runs the merge and reports it: drift check (exit 1 on drift), dry-run preview, or apply.
func vscodeSync(o vscode.SyncOptions, check, dryRun bool) int {
	if check && dryRun {
		fmt.Fprintln(os.Stderr, "claude-code: theme vscode --check and --dry-run are mutually exclusive")
		return 2
	}
	o.Check = check || dryRun
	out, err := vscode.SyncAll(o)
	if err != nil {
		return helptext.Fail("vscode: %v", err)
	}
	r := vscodeReport{out}
	switch {
	case check:
		return r.check()
	case dryRun:
		r.dryRun()
	default:
		r.apply()
	}
	return 0
}

// vscodeReport formats one sync run's outcome.
type vscodeReport struct{ vscode.Outcome }

// check prints DRIFT lines and returns 1 when anything would change, else reports in-sync and returns 0.
func (o vscodeReport) check() int {
	drift := false
	if o.Result.Changed {
		fmt.Fprintf(os.Stderr, "claude-code: vscode DRIFT %s\n", o.DestPath)
		drift = true
	}
	if !o.Skip && o.KbResult.Changed {
		fmt.Fprintf(os.Stderr, "claude-code: vscode DRIFT %s\n", o.KbDestPath)
		drift = true
	}
	if o.ExtResult.Changed {
		fmt.Fprintf(os.Stderr, "claude-code: vscode DRIFT %s\n", o.ExtResult.Target)
		drift = true
	}
	if drift {
		return 1
	}
	o.reportMerges()
	fmt.Printf("claude-code: vscode extension in sync %s\n", o.ExtResult.Target)
	return 0
}

func (o vscodeReport) dryRun() {
	reportDryRun("template", o.DestPath, o.Result.Changed)
	reportKeybindings(o.Skip, o.Note, func() { reportDryRun("keybindings template", o.KbDestPath, o.KbResult.Changed) })
	reportDryRun("extension", o.ExtResult.Target, o.ExtResult.Changed)
}

func (o vscodeReport) apply() {
	o.reportMerges()
	if o.ExtResult.Changed {
		fmt.Printf("claude-code: vscode linked extension -> %s\n", o.ExtResult.Target)
	} else {
		fmt.Printf("claude-code: vscode extension in sync %s\n", o.ExtResult.Target)
	}
}

func (o vscodeReport) reportMerges() {
	reportMerge("template", o.DestPath, o.Result)
	reportKeybindings(o.Skip, o.Note, func() { reportMerge("keybindings template", o.KbDestPath, o.KbResult) })
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
