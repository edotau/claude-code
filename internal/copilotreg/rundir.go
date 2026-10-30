package copilotreg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// RegistryFile is the 0600 registry file of the process pid inside the runtime dir.
func RegistryFile(dir string, pid int) string { return filepath.Join(dir, strconv.Itoa(pid)+".json") }

// WriteRegistry atomically writes data as <pid>.json (0600) in a verified runtime dir and returns its path.
func WriteRegistry(pid int, data []byte) (string, error) {
	dir, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	file := RegistryFile(dir, pid)
	if err := paths.AtomicWrite(file, data, 0o600); err != nil {
		return "", fmt.Errorf("copilotreg: write registry: %w", err)
	}
	return file, nil
}

// RuntimeDir creates (0700) and verifies the registry directory: a real dir, owned by this uid, mode 0700,
// and memory-backed where the platform allows (see backingCheck).
func RuntimeDir() (string, error) {
	dir, err := runtimeDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("copilotreg: runtime dir: %w", err)
	}
	if err := checkDir(dir); err != nil {
		return "", err
	}
	if err := backingCheck(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// checkDir fails unless dir is a non-symlink directory owned by the current uid with mode exactly 0700.
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("copilotreg: %s is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("copilotreg: %s has mode %o, want 0700", dir, perm)
	}
	if uid, ok := ownerUID(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("copilotreg: %s is not owned by uid %d", dir, os.Getuid())
	}
	return nil
}

var errNoRuntimeDir = errors.New("copilotreg: no private runtime directory on this platform")

// pidOf parses "<pid>.json"; ok is false for any other name.
func pidOf(path string) (int, bool) {
	name, ok := strings.CutSuffix(filepath.Base(path), ".json")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(name)
	return pid, err == nil && pid > 0
}

// Keep reports whether the registry file at path must survive a sweep: its pid is alive, or it is not ours.
func Keep(path string) bool {
	pid, ok := pidOf(path)
	return !ok || pidAlive(pid)
}

// Glob is the pattern matching registry files under the runtime dir; "" when no dir path exists. It creates nothing.
func Glob() string {
	dir, err := runtimeDirPath()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "*.json")
}
