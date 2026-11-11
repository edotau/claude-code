package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// wslPowerShell opens a URL from the environment, so its & and ? never pass through a Windows command line.
const wslPowerShell = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"

// wslExplorer opens a URL in the Windows default browser; absolute because ssh sessions lack the Windows PATH.
const wslExplorer = "/mnt/c/Windows/explorer.exe"

// Seams: tests swap the OS and the WSL answer to reach every opener from any host.
var (
	openGOOS = runtime.GOOS
	openWSL  = OnWSL
)

// OpenURL starts the desktop opener without waiting: explorer.exe exits 1 even when it opens the page.
func OpenURL(url string) error {
	return openCmd(url, Opener()).Start() // the opener outlives this call
}

// openCmd is opener run on url; WSL's explorer.exe fallback becomes PowerShell when Windows has it.
func openCmd(url, opener string) *exec.Cmd {
	if _, err := os.Stat(wslPowerShell); opener != wslExplorer || err != nil {
		return exec.CommandContext(context.Background(), opener, url)
	}
	cmd := exec.CommandContext(context.Background(), wslPowerShell, "-NoProfile", "-NonInteractive",
		"-Command", "Start-Process $env:WIN_BROWSER_URL")
	wslenv := "WIN_BROWSER_URL"
	if v := os.Getenv("WSLENV"); v != "" {
		wslenv = v + ":" + wslenv
	}
	cmd.Env = append(os.Environ(), "WIN_BROWSER_URL="+url, "WSLENV="+wslenv)
	if sock := wslInterop(); sock != "" {
		cmd.Env = append(cmd.Env, "WSL_INTEROP="+sock)
	}
	return cmd
}

// wslInterop is the newest WSL interop socket when the session has none (Remote-SSH, Tailscale ssh), else "".
func wslInterop() string {
	if fi, err := os.Stat(os.Getenv("WSL_INTEROP")); err == nil && fi.Mode()&os.ModeSocket != 0 {
		return ""
	}
	socks, _ := filepath.Glob("/run/WSL/*_interop")
	newest, at := "", int64(0)
	for _, s := range socks {
		if fi, err := os.Stat(s); err == nil && fi.ModTime().UnixNano() > at {
			newest, at = s, fi.ModTime().UnixNano()
		}
	}
	return newest
}

// Opener is this machine's URL opener: open on macOS, wslview or explorer.exe on WSL, else xdg-open.
func Opener() string {
	switch {
	case openGOOS == "darwin":
		return "open"
	case openWSL():
		if p, err := exec.LookPath("wslview"); err == nil {
			return p
		}
		return wslExplorer
	}
	return "xdg-open"
}
