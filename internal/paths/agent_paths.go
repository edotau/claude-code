package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

func CodexConfigPath(home string) (string, error) {
	if home != "" {
		return filepath.Abs(filepath.Join(home, ".codex", "config.toml"))
	}
	dir, err := CodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// CodexHome is $CODEX_HOME (made absolute) or ~/.codex, the directory holding codex's config.toml and auth.json.
func CodexHome() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// CopilotHome is $COPILOT_HOME unchanged, or ~/.copilot when the override is empty.
func CopilotHome() (string, error) {
	if dir := os.Getenv("COPILOT_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".copilot"), nil
}

// OpencodeDataDir is $XDG_DATA_HOME/opencode (default ~/.local/share/opencode).
func OpencodeDataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode")
}

// OpencodeDBPath is opencode's SQLite database (written since 1.x); "" for an in-memory DB; OPENCODE_DB overrides the stable channel default.
func OpencodeDBPath() string {
	db := os.Getenv("OPENCODE_DB")
	switch {
	case db == ":memory:":
		return ""
	case filepath.IsAbs(db):
		return db
	case db == "":
		db = "opencode.db"
	}
	return filepath.Join(OpencodeDataDir(), db)
}

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
				return "", errors.New("copilot registry: no private runtime directory on this platform: XDG_RUNTIME_DIR is unset")
			}
		}
		if !filepath.IsAbs(base) {
			return "", fmt.Errorf("copilot registry: XDG_RUNTIME_DIR %q is not absolute", base)
		}
		return filepath.Join(base, "claude-code", "copilot"), nil
	}
	if runtime.GOOS != "darwin" {
		return "", errors.New("copilot registry: no private runtime directory on this platform")
	}
	base := os.Getenv("TMPDIR")
	if !filepath.IsAbs(base) {
		return "", errors.New("copilot registry: no private runtime directory on this platform: TMPDIR is unset")
	}
	return filepath.Join(base, "claude-code-"+strconv.Itoa(os.Getuid()), "copilot"), nil
}
