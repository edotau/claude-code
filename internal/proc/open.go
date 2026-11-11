package proc

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Windows programs under the WSL drive mount; absolute because ssh sessions lack the Windows PATH.
const (
	winPowerShell = "c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
	winExplorer   = "c/Windows/explorer.exe"
)

// Seams: tests swap the OS, the WSL answer and wsl.conf to reach every opener from any host.
var (
	openGOOS = runtime.GOOS
	openWSL  = OnWSL
	wslConf  = "/etc/wsl.conf"
)

// wslMount is where WSL mounts the Windows drives: wsl.conf's automount root, else /mnt.
func wslMount() string {
	f, err := os.Open(wslConf)
	if err != nil {
		return "/mnt"
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if v = strings.Trim(strings.TrimSpace(v), `"`); ok && strings.TrimSpace(k) == "root" && v != "" {
			return v
		}
	}
	return "/mnt"
}

// OpenURL opens url on this desktop; only WSL's PowerShell is waited for, so only it can report a failed open.
func OpenURL(url string) error {
	ps := filepath.Join(wslMount(), winPowerShell)
	if _, err := os.Stat(ps); openGOOS == "darwin" || !openWSL() || err != nil {
		return exec.CommandContext(context.Background(), Opener(), url).Start() // the opener outlives this call
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := powerShellOpen(ctx, ps, url).CombinedOutput(); err != nil {
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return fmt.Errorf("powershell could not open the Windows browser: %w: %s", err, strings.TrimSpace(line))
	}
	return nil
}

// powerShellOpen starts url from the environment, so its & and ? never pass through a Windows command line.
func powerShellOpen(ctx context.Context, ps, url string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-Command",
		"$ErrorActionPreference='Stop'; Start-Process $env:WIN_BROWSER_URL")
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

// Opener is the one-argument URL opener: open on macOS, wslview or explorer.exe on WSL, else xdg-open.
func Opener() string {
	switch {
	case openGOOS == "darwin":
		return "open"
	case openWSL():
		if p, err := exec.LookPath("wslview"); err == nil {
			return p
		}
		return filepath.Join(wslMount(), winExplorer)
	}
	return "xdg-open"
}
