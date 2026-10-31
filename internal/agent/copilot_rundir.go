package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// CopilotRegistryFile is the 0600 registry file of the process pid inside the runtime dir.
func CopilotRegistryFile(dir string, pid int) string {
	return filepath.Join(dir, strconv.Itoa(pid)+".json")
}

// WriteCopilotRegistry atomically writes data as <pid>.json (0600) in a verified runtime dir and returns its path.
func WriteCopilotRegistry(pid int, data []byte) (string, error) {
	dir, err := CopilotRuntimeDir()
	if err != nil {
		return "", err
	}
	file := CopilotRegistryFile(dir, pid)
	if err := paths.AtomicWrite(file, data, 0o600); err != nil {
		return "", fmt.Errorf("copilot registry: write registry: %w", err)
	}
	return file, nil
}

// CopilotRuntimeDir creates (0700) and verifies the registry directory: a real dir, owned by this uid, mode 0700,
// and memory-backed where the platform allows (see backingCheck).
func CopilotRuntimeDir() (string, error) {
	dir, err := runtimeDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("copilot registry: runtime dir: %w", err)
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
		return fmt.Errorf("copilot registry: %s is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("copilot registry: %s has mode %o, want 0700", dir, perm)
	}
	if uid, ok := ownerUID(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("copilot registry: %s is not owned by uid %d", dir, os.Getuid())
	}
	return nil
}

var errNoRuntimeDir = errors.New("copilot registry: no private runtime directory on this platform")

// pidOf parses "<pid>.json"; ok is false for any other name.
func pidOf(path string) (int, bool) {
	name, ok := strings.CutSuffix(filepath.Base(path), ".json")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(name)
	return pid, err == nil && pid > 0
}

// CopilotRegistryKeep reports whether the registry file at path must survive a sweep: its pid is alive, or it is not ours.
func CopilotRegistryKeep(path string) bool {
	pid, ok := pidOf(path)
	return !ok || proc.Alive(pid) || runtime.GOOS == "windows"
}

// CopilotRegistryGlob is the pattern matching registry files under the runtime dir; "" when no dir path exists. It creates nothing.
func CopilotRegistryGlob() string {
	dir, err := runtimeDirPath()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "*.json")
}
