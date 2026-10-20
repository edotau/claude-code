package browser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// NewEnv builds the real Env with actual paths, bounded commands, and time.Now.
func NewEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{
		Home:       home,
		AppSupport: appSupportRoot(home, runtime.GOOS),
		Run:        runReal,
		Now:        time.Now,
		GOOS:       runtime.GOOS,
		WSL:        runtime.GOOS == "linux" && isWSL(),
		WinRoot:    winRoot(),
		Probe:      probeCDP,
		Sleep:      time.Sleep,
		ClaudeBin:  realClaude(),
		HasNPX:     onPath("npx"),
	}
}

// appSupportRoot is macOS's "~/Library/Application Support" or Linux's "~/.config".
func appSupportRoot(home, goos string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(home, ".config")
}

// runReal bounds detection/setup commands and returns trimmed stdout.
func runReal(name string, args ...string) (string, error) {
	return runCommand(30*time.Second, name, args...)
}

func runCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// realClaude resolves the vendor claude binary, or "" on error.
func realClaude() string {
	p, _ := paths.VendorBin("claude")
	return p
}

// winRoot is where WSL mounts C: (wslpath honors a custom /etc/wsl.conf automount root), else /mnt/c.
func winRoot() string {
	if out, err := runCommand(2*time.Second, "wslpath", "-u", `C:\`); err == nil {
		if p := strings.TrimRight(out, "/"); p != "" {
			return p
		}
	}
	return "/mnt/c"
}

// onPath reports whether name resolves on PATH.
func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
