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

// commandTimeout bounds one detection command. 30s covers a slow setup probe; tests set it lower.
var commandTimeout = 30 * time.Second

// SetCommandTimeout overrides commandTimeout for a test and returns the restore; production keeps 30s.
func SetCommandTimeout(d time.Duration) (restore func()) {
	saved := commandTimeout
	commandTimeout = d
	return func() { commandTimeout = saved }
}

// runReal bounds detection/setup commands and returns trimmed stdout.
func runReal(name string, args ...string) (string, error) {
	return runCommand(commandTimeout, name, args...)
}

func runCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if name == "powershell.exe" && !onPath(name) { // interop left Windows off PATH (VS Code Remote-WSL terminals)
		name = filepath.Join(winRoot(), "Windows", "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// realClaude resolves the agent claude binary, or "" on error.
func realClaude() string {
	p, _ := paths.VendorBin("claude")
	return p
}

// winRoot is where WSL mounts C: (the mount table honors a custom /etc/wsl.conf automount root), else /mnt/c.
func winRoot() string { return winRootFrom(paths.Default()) }

// winRootFrom is winRoot over an explicit resolver.
func winRootFrom(r *paths.Resolver) string {
	if p, ok := r.Resolve(`C:\`); ok && p != "/" {
		return p
	}
	return "/mnt/c"
}

// onPath reports whether name resolves on PATH.
func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
