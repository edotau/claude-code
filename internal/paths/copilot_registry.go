package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

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

// CopilotRegistryGlob matches registry files under the runtime dir; "" when none exists; it creates nothing.
func CopilotRegistryGlob() string {
	dir, err := CopilotRuntimeDirPath()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "*.json")
}

// CopilotRuntimeDirPath is the registry directory: tmpfs $XDG_RUNTIME_DIR on linux, per-user $TMPDIR on darwin; no I/O.
func CopilotRuntimeDirPath() (string, error) {
	if runtime.GOOS == "linux" {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
			if _, err := os.Stat(base); err != nil {
				return "", fmt.Errorf("copilot registry: no private runtime directory on this platform: XDG_RUNTIME_DIR is unset")
			}
		}
		if !filepath.IsAbs(base) {
			return "", fmt.Errorf("copilot registry: XDG_RUNTIME_DIR %q is not absolute", base)
		}
		return filepath.Join(base, "claude-code", "copilot"), nil
	}
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("copilot registry: no private runtime directory on this platform")
	}
	base := os.Getenv("TMPDIR")
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("copilot registry: no private runtime directory on this platform: TMPDIR is unset")
	}
	return filepath.Join(base, "claude-code-"+strconv.Itoa(os.Getuid()), "copilot"), nil
}
