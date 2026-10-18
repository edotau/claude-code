package browser

import (
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
