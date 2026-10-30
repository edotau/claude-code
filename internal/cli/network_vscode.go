package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/network/vscode"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"os"
	"os/exec"
	"path/filepath"
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
		return vscodeServeStart(ctx, vscode.Options{Port: *port, NoTailscale: *noTailscale, Funnel: *funnel, Folder: abs}, *format)
	}
}

func vscodeServeStart(ctx context.Context, opts vscode.Options, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return fail("network vscode: %v", err)
	}
	res, err := vscode.Start(ctx, opts)
	if err != nil {
		return fail("network vscode: %v", err)
	}
	for _, n := range res.Notes {
		fmt.Fprintln(os.Stderr, "claude-code: network vscode: "+n)
	}
	if format == "json" {
		if err := helptext.WriteJSON(os.Stdout, res.State); err != nil {
			return fail("network vscode: %v", err)
		}
		return 0
	}
	fmt.Printf("claude-code: network vscode running pid %d port %d\n", res.State.PID, res.State.Port)
	fmt.Println(res.State.URL)
	printQR(res.State.URL)
	return 0
}

func vscodeServeStop(format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return fail("network vscode: %v", err)
	}
	st, wasRunning := vscode.Running()
	err := vscode.Stop()
	if errors.Is(err, vscode.ErrNotRunning) || (err == nil && !wasRunning) {
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
		b, err := json.Marshal(struct {
			Stopped bool `json:"stopped"`
			PID     int  `json:"pid"`
		}{true, st.PID})
		if err != nil {
			return fail("network vscode: %v", err)
		}
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("claude-code: network vscode: stopped pid %d\n", st.PID)
	return 0
}

// vscodeServeStatus reports the daemon from our own state plus a live re-query of `tailscale serve
// status --json` for the mapping — the source of truth, since the daemon may have started earlier.
func vscodeServeStatus(ctx context.Context, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return fail("network vscode: %v", err)
	}
	st, running := vscode.Running()
	_, mapped := vscode.TailscaleMapping(ctx, st.Port)
	if format == "json" {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Running bool `json:"running"`
			Mapped  bool `json:"tailscale_mapped"`
			vscode.State
		}{running, mapped, st}); err != nil {
			return fail("network vscode: %v", err)
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
