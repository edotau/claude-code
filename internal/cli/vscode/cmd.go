package vscode

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/cli/ui/helptext"
)

func reportMerge(label, dest string, result mergeResult) {
	switch {
	case !result.Changed:
		fmt.Printf("claude-code: vscode %s in sync %s\n", label, dest)
	case result.Backup == "":
		fmt.Printf("claude-code: vscode merged %s -> %s\n", label, dest)
	default:
		fmt.Printf("claude-code: vscode merged %s -> %s (backup %s)\n", label, dest, result.Backup)
	}
}

// Run is `configure vscode`: flags only, it merges the settings and keybindings templates.
func Run(args []string) int {
	fs := helptext.NewFlagSet("configure vscode", helptext.Page{Usage: "claude-code configure vscode [flags]",
		About: "Merge embedded templates into VS Code's settings.json and keybindings.\n" +
			"settings.json is the User scope, which Settings Sync syncs; under\n" +
			"~/.vscode-server it is the Machine scope, which Settings Sync skips.\n" +
			"Skip keybindings on headless boxes with only ~/.vscode-server."})
	check := fs.Bool("check", false, "report drift without writing; exit 1 on drift")
	dryRun := fs.Bool("dry-run", false, "preview the merge, no write")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 { // a stray argument must not fall through to a merge
		return helptext.Fail("configure vscode: unexpected argument %q", fs.Arg(0))
	}
	return vscodeSync(syncOptions{}, *check, *dryRun)
}

// vscodeSync runs the merge and reports it: drift check (exit 1 on drift), dry-run preview, or apply.
func vscodeSync(o syncOptions, check, dryRun bool) int {
	if check && dryRun {
		fmt.Fprintln(os.Stderr, "claude-code: configure vscode --check and --dry-run are mutually exclusive")
		return 2
	}
	o.Check = check || dryRun
	out, err := syncAll(o)
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
type vscodeReport struct{ outcome }

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
	if drift {
		return 1
	}
	o.apply()
	return 0
}

func (o vscodeReport) dryRun() {
	reportDryRun("template", o.DestPath, o.Result.Changed)
	reportSkip("keybindings", o.Skip, o.Note, func() { reportDryRun("keybindings template", o.KbDestPath, o.KbResult.Changed) })
}

func (o vscodeReport) apply() {
	reportMerge("template", o.DestPath, o.Result)
	reportSkip("keybindings", o.Skip, o.Note, func() { reportMerge("keybindings template", o.KbDestPath, o.KbResult) })
}

// reportSkip prints label's skip note, or calls report when that part ran.
func reportSkip(label string, skip bool, note string, report func()) {
	if skip {
		fmt.Printf("claude-code: vscode %s skipped: %s\n", label, note)
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

// Spec is the completion node for `configure vscode`.
func Spec() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{helptext.BoolFlag("check"), helptext.BoolFlag("dry-run")}}
}
