package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vscode"
	"github.com/edotau/claude-code/internal/vscodeserve"
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

	if *check {
		return vscodeCheckReport(destPath, result, kbDestPath, skip, note, kbResult, extResult)
	}
	if *dryRun {
		vscodeDryRunReport(destPath, result, kbDestPath, skip, note, kbResult, extResult)
		return 0
	}
	vscodeApplyReport(destPath, result, kbDestPath, skip, note, kbResult, extResult)
	return 0
}

func resolveVSCodeDest(dest string) (string, error) {
	if dest != "" {
		return dest, nil
	}
	return vscode.DefaultDest()
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

func vscodeCheckReport(destPath string, result vscode.Result, kbDestPath string, skip bool, note string, kbResult vscode.Result, extResult vscode.ExtensionResult) int {
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
	return 0
}

func vscodeDryRunReport(destPath string, result vscode.Result, kbDestPath string, skip bool, note string, kbResult vscode.Result, extResult vscode.ExtensionResult) {
	reportDryRun("template", destPath, result.Changed)
	reportKeybindings(skip, note, func() { reportDryRun("keybindings template", kbDestPath, kbResult.Changed) })
	reportDryRun("extension", extResult.Target, extResult.Changed)
}

func vscodeApplyReport(destPath string, result vscode.Result, kbDestPath string, skip bool, note string, kbResult vscode.Result, extResult vscode.ExtensionResult) {
	reportMerge("template", destPath, result)
	reportKeybindings(skip, note, func() { reportMerge("keybindings template", kbDestPath, kbResult) })
	if extResult.Changed {
		fmt.Printf("claude-code: vscode linked extension -> %s\n", extResult.Target)
	} else {
		fmt.Printf("claude-code: vscode extension in sync %s\n", extResult.Target)
	}
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

// cmdNetworkVSCode runs `code serve-web` as a detached daemon and maps it onto the tailnet.
func cmdNetworkVSCode(args []string) int {
	fs := helptext.NewFlagSet("network vscode", helptext.Page{Usage: "claude-code network vscode [flags]",
		About: "Publish VS Code's web UI (code serve-web) to the phone over the tailnet.\n" +
			"The browser opens --folder (default: the current directory) with the\n" +
			"desktop's extensions.\n" +
			"--funnel maps it on the public internet instead of the tailnet (still\n" +
			"token-gated)."})
	port := fs.Int("port", vscodeserve.DefaultPort, "serve-web port; falls back to the next free one when busy")
	stop := fs.Bool("stop", false, "stop the daemon and remove any tailscale/funnel mapping")
	status := fs.Bool("status", false, "report the daemon and tailscale mapping, then exit")
	noTailscale := fs.Bool("no-tailscale", false, "serve on 127.0.0.1 only; skip the tailscale mapping")
	funnel := fs.Bool("funnel", false, "use tailscale funnel, not serve (public internet, token-gated)")
	folder := fs.String("folder", ".", "folder the browser opens by default")
	format := fs.String("format", "text", "output format: text|json")
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
			return fail("network vscode: %v", err)
		}
		return vscodeServeStart(ctx, vscodeserve.Options{Port: *port, NoTailscale: *noTailscale, Funnel: *funnel, Folder: abs}, *format)
	}
}

func vscodeServeStart(ctx context.Context, opts vscodeserve.Options, format string) int {
	res, err := vscodeserve.Start(ctx, opts)
	if err != nil {
		return fail("network vscode: %v", err)
	}
	for _, n := range res.Notes {
		fmt.Fprintln(os.Stderr, "claude-code: network vscode: "+n)
	}
	if format == "json" {
		_ = outfmt.Write(os.Stdout, res.State)
		return 0
	}
	fmt.Printf("claude-code: network vscode running pid %d port %d\n", res.State.PID, res.State.Port)
	fmt.Println(res.State.URL)
	printQR(res.State.URL)
	return 0
}

func vscodeServeStop(format string) int {
	st, wasRunning := vscodeserve.Running()
	err := vscodeserve.Stop()
	if errors.Is(err, vscodeserve.ErrNotRunning) || (err == nil && !wasRunning) {
		if format == "json" {
			fmt.Println(`{"running":false}`)
		} else {
			fmt.Println("claude-code: network vscode: not running")
		}
		return 0
	}
	if err != nil {
		return fail("network vscode: %v", err)
	}
	if format == "json" {
		b, _ := json.Marshal(struct {
			Stopped bool `json:"stopped"`
			PID     int  `json:"pid"`
		}{true, st.PID})
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("claude-code: network vscode: stopped pid %d\n", st.PID)
	return 0
}

// vscodeServeStatus reports the daemon from our own state plus a live re-query of `tailscale serve
// status --json` for the mapping — the source of truth, since the daemon may have started earlier.
func vscodeServeStatus(ctx context.Context, format string) int {
	st, running := vscodeserve.Running()
	_, mapped := vscodeserve.TailscaleMapping(ctx, st.Port)
	if format == "json" {
		_ = outfmt.Write(os.Stdout, struct {
			Running bool `json:"running"`
			Mapped  bool `json:"tailscale_mapped"`
			vscodeserve.State
		}{running, mapped, st})
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
