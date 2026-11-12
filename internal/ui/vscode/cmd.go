package vscode

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/ui/helptext"
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

// Run merges the tracked VS Code settings template into VS Code's settings.json.
// That is the User scope (Settings Sync syncs it) except under ~/.vscode-server, the unsynced Machine scope.
func Run(args []string) int {
	fs := helptext.NewFlagSet("theme vscode", helptext.Page{Usage: "claude-code theme vscode [flags]",
		About: "Merge embedded templates into VS Code's settings.json and keybindings.\n" +
			"settings.json is the User scope, which Settings Sync syncs; under\n" +
			"~/.vscode-server it is the Machine scope, which Settings Sync skips.\n" +
			"Skip keybindings on headless boxes with only ~/.vscode-server.\n" +
			"Link the Claude Code Agents extension into the detected extensions directory.\n" +
			"Write one chatLanguageModels.json group per signed-in Databricks profile."})
	var o syncOptions
	fs.StringVar(&o.Template, "template", "", "settings template to merge (default: the one embedded in the binary)")
	fs.StringVar(&o.Dest, "dest", "", "VS Code settings.json (default: platform-detected)")
	fs.StringVar(&o.ExtensionsDir, "extensions-dir", "", "link the Agents extension here (default: platform-detected)")
	fs.StringVar(&o.KeybindingsTemplate, "keybindings-template", "", "keybindings template to merge (default: the one embedded in the binary)")
	fs.StringVar(&o.KeybindingsDest, "keybindings-dest", "", "VS Code User keybindings.json (default: platform-detected)")
	fs.StringVar(&o.ModelsDest, "models-dest", "", "chatLanguageModels.json (default: beside --dest)")
	check := fs.Bool("check", false, "report drift without writing; exit 1 on drift")
	dryRun := fs.Bool("dry-run", false, "preview the merge, no write (not with --check)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 { // a stray argument must not fall through to a merge
		return helptext.Fail("theme vscode: unexpected argument %q", fs.Arg(0))
	}
	return vscodeSync(o, *check, *dryRun)
}

// vscodeSync runs the merge and reports it: drift check (exit 1 on drift), dry-run preview, or apply.
func vscodeSync(o syncOptions, check, dryRun bool) int {
	if check && dryRun {
		fmt.Fprintln(os.Stderr, "claude-code: theme vscode --check and --dry-run are mutually exclusive")
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
	if o.ExtResult.Changed {
		fmt.Fprintf(os.Stderr, "claude-code: vscode DRIFT %s\n", o.ExtResult.Target)
		drift = true
	}
	if !o.Models.Skip && o.Models.Result.Changed {
		fmt.Fprintf(os.Stderr, "claude-code: vscode DRIFT %s\n", o.Models.Dest)
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
	reportSkip("keybindings", o.Skip, o.Note, func() { reportDryRun("keybindings template", o.KbDestPath, o.KbResult.Changed) })
	reportDryRun("extension", o.ExtResult.Target, o.ExtResult.Changed)
	reportSkip("chat models", o.Models.Skip, o.Models.Note, func() { reportDryRun("chat models", o.Models.Dest, o.Models.Result.Changed) })
	o.reportGroups()
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
	reportSkip("keybindings", o.Skip, o.Note, func() { reportMerge("keybindings template", o.KbDestPath, o.KbResult) })
	reportSkip("chat models", o.Models.Skip, o.Models.Note, func() { reportMerge("chat models", o.Models.Dest, o.Models.Result) })
	o.reportGroups()
}

// reportGroups lists the Databricks model-picker groups the chat models merge owns.
func (o vscodeReport) reportGroups() {
	if !o.Models.Skip && len(o.Models.Groups) > 0 {
		fmt.Printf("claude-code: vscode chat model groups: %s\n", strings.Join(o.Models.Groups, ", "))
	}
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

// Spec is the completion node for `theme vscode`.
func Spec() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{helptext.Plain("template"), helptext.Plain("dest"), helptext.Plain("extensions-dir"),
		helptext.Plain("keybindings-template"), helptext.Plain("keybindings-dest"), helptext.Plain("models-dest"), helptext.BoolFlag("check"), helptext.BoolFlag("dry-run")}}
}
