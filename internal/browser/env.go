package browser

import (
	"github.com/edotau/claude-code/internal/paths"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// NewEnv builds the real Env: the actual home dir, Application Support root, exec.Command, time.Now.
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

// runReal runs name with args and returns trimmed stdout; the real Env.Run, never called in tests.
func runReal(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// realClaude resolves the claude binary past our own shim dir, or "" (then plain "claude" on PATH).
func realClaude() string {
	p, _ := paths.LookPathReal("claude")
	return p
}

// winRoot is where WSL mounts C: (wslpath honors a custom /etc/wsl.conf automount root), else /mnt/c.
func winRoot() string {
	if out, err := exec.Command("wslpath", "-u", `C:\`).Output(); err == nil {
		if p := strings.TrimRight(strings.TrimSpace(string(out)), "/"); p != "" {
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
