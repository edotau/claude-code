package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/edotau/claude-code/internal/network/vscode"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdNetworkVSCode runs `code serve-web` as a detached daemon and maps it onto the tailnet.
func cmdNetworkVSCode(args []string) int {
	fs := helptext.NewFlagSet("network vscode", helptext.Page{Usage: "claude-code network vscode [flags]",
		About: "Publish VS Code's web UI (code serve-web) to the phone over the tailnet.\n" +
			"The browser opens --folder (default: the current directory) with the\n" +
			"desktop's extensions.\n" +
			"--funnel maps it on the public internet instead of the tailnet (still\n" +
			"token-gated)."})
	port := fs.Int("port", vscode.DefaultPort, "serve-web port; falls back to the next free one when busy")
	stop := fs.Bool("stop", false, "stop the daemon and remove any tailscale/funnel mapping")
	status := fs.Bool("status", false, "report the daemon and tailscale mapping, then exit")
	noTailscale := fs.Bool("no-tailscale", false, "serve on 127.0.0.1 only; skip the tailscale mapping")
	funnel := fs.Bool("funnel", false, "use tailscale funnel, not serve (public internet, token-gated)")
	folder := fs.String("folder", ".", "folder the browser opens by default")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	ctx := context.Background()
	switch {
	case *stop:
		return vscodeServeStop(*format)
	case *status:
		return vscodeServeStatus(ctx, *format)
	default:
		abs, err := filepath.Abs(*folder)
		if err != nil {
			return helptext.Fail("network vscode: %v", err)
		}
		return vscodeServeStart(ctx, vscode.Options{Port: *port, NoTailscale: *noTailscale, Funnel: *funnel, Folder: abs}, *format)
	}
}

func vscodeServeStart(ctx context.Context, opts vscode.Options, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("network vscode", err)
	}
	res, err := vscode.Start(ctx, opts)
	if err != nil {
		return helptext.Fail("network vscode: %v", err)
	}
	for _, n := range res.Notes {
		fmt.Fprintln(os.Stderr, "claude-code: network vscode: "+n)
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("network vscode", res.State)
	}
	fmt.Printf("claude-code: network vscode running pid %d port %d\n", res.State.PID, res.State.Port)
	fmt.Println(res.State.URL)
	printQR(res.State.URL)
	return 0
}

func vscodeServeStop(format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("network vscode", err)
	}
	st, wasRunning := vscode.Running()
	err := vscode.Stop()
	if errors.Is(err, vscode.ErrNotRunning) || (err == nil && !wasRunning) {
		if format == helptext.JSON {
			return helptext.WriteJSONOrFail("network vscode", map[string]bool{"running": false})
		} else {
			fmt.Println("claude-code: network vscode: not running")
		}
		return 0
	}
	if err != nil {
		return helptext.Fail("network vscode: %v", err)
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("network vscode", struct {
			Stopped bool `json:"stopped"`
			PID     int  `json:"pid"`
		}{true, st.PID})
	}
	fmt.Printf("claude-code: network vscode: stopped pid %d\n", st.PID)
	return 0
}

// vscodeServeStatus reports the daemon from our state plus a live `tailscale serve status --json` re-query.
func vscodeServeStatus(ctx context.Context, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("network vscode", err)
	}
	st, running := vscode.Running()
	_, mapped := vscode.TailscaleMapping(ctx, st.Port)
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Running bool `json:"running"`
			Mapped  bool `json:"tailscale_mapped"`
			vscode.State
		}{running, mapped, st}); err != nil {
			return helptext.Fail("network vscode: %v", err)
		}
		if !running {
			return 1
		}
		return 0
	}
	if !running {
		fmt.Println("claude-code: network vscode: not running")
		return 1
	}
	fmt.Printf("claude-code: network vscode running pid %d port %d tailscale=%v funnel=%v mapped=%v\n",
		st.PID, st.Port, st.Tailscale, st.Funnel, mapped)
	fmt.Println(st.URL)
	return 0
}

// printQR prints a terminal QR code for url when `qrencode` is on PATH; a one-line hint otherwise.
func printQR(url string) {
	bin, err := exec.LookPath("qrencode")
	if err != nil {
		fmt.Println("claude-code: network vscode: install `qrencode` for a QR code (e.g. `brew install qrencode`)")
		return
	}
	cmd := exec.Command(bin, "-t", "ANSIUTF8", url) //nolint:noctx // one-shot presentation helper, not a daemon.
	cmd.Stdout = os.Stdout
	_ = cmd.Run()
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
	if *check && *dryRun {
		fmt.Fprintln(os.Stderr, "claude-code: theme vscode --check and --dry-run are mutually exclusive")
		return 2
	}
	o.Check = *check || *dryRun
	out, err := vscode.SyncAll(o)
	if err != nil {
		return helptext.Fail("vscode: %v", err)
	}
	r := vscodeReport{out}
	switch {
	case *check:
		return r.check()
	case *dryRun:
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
