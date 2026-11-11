package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/settings"
)

// windowsProfileEnv overrides the resolved %USERPROFILE%, for tests and live checks (HARNESS_ convention).
const windowsProfileEnv = "HARNESS_WINDOWS_PROFILE"

// cmdExeTimeout bounds the cmd.exe interop call so a hung Windows side never stalls install/generate.
const cmdExeTimeout = 5 * time.Second

// windowsOnlyTopKeys never belong in the Windows settings file: no hook runner or statusline script there.
var windowsOnlyTopKeys = []string{"hooks", "statusLine"}

// linuxPathMarkers flag a string value as WSL-only.
var linuxPathMarkers = []string{"/home/", "/usr/", "$HOME", "~/.claude/bin"}

// onWSL reports a WSL host; a seam so tests run the WSL and non-WSL branches on any machine.
var onWSL = proc.OnWSL

// windowsProfileDir is the WSL path of the Windows user profile (%USERPROFILE%), or "" when it can't
// be resolved (off WSL, cmd.exe unavailable, or the profile does not exist).
func windowsProfileDir() string {
	if v := strings.TrimSpace(os.Getenv(windowsProfileEnv)); v != "" {
		return v
	}
	if !onWSL() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmdExeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "cmd.exe", "/c", "echo %USERPROFILE%").Output()
	if err != nil {
		return ""
	}
	win := strings.TrimSpace(string(out))
	if win == "" || win == "%USERPROFILE%" {
		return ""
	}
	dir, _ := paths.Resolve(win)
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// installWindowsSettings seeds a missing Windows settings.json with the Windows subset of the template render;
// an existing one is never written. A no-op off WSL, or a one-line skip note when the profile can't be resolved.
func installWindowsSettings(w io.Writer, dryRun bool) error {
	if !onWSL() && os.Getenv(windowsProfileEnv) == "" {
		return nil
	}
	dir := windowsProfileDir()
	if dir == "" {
		fmt.Fprintf(w, "settings (windows) skip  %s not resolved (cmd.exe unavailable?)\n", "%USERPROFILE%")
		return nil
	}
	file := filepath.Join(dir, ".claude", "settings.json")
	if _, err := os.Stat(file); err == nil || !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			fmt.Fprintln(w, "settings (windows) kept (yours)")
		}
		return err
	}
	if dryRun {
		fmt.Fprintln(w, "settings (windows) missing · would seed; --dry-run wrote nothing")
		return nil
	}
	seeded, err := settings.SeedSettings(file, stripWindowsOnly)
	if seeded {
		fmt.Fprintln(w, "settings (windows) seeded from the template")
	}
	return err
}

// isLinuxString reports a JSON string value naming a Linux path (not an array/object value).
func isLinuxString(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	for _, m := range linuxPathMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// stripWindowsOnly drops hooks/statusLine and any plain string value (top-level, or inside env) naming
// a Linux path, keeping everything else: plain keys, permissions, enabledPlugins, marketplaces.
func stripWindowsOnly(rendered []byte) ([]byte, error) {
	return settings.FilterObject(rendered, func(key string, val json.RawMessage) (json.RawMessage, bool, error) {
		switch {
		case slices.Contains(windowsOnlyTopKeys, key):
			return nil, false, nil
		case key == "env":
			filtered, err := settings.FilterObject(val, func(_ string, v json.RawMessage) (json.RawMessage, bool, error) {
				return v, !isLinuxString(v), nil
			})
			if err != nil {
				return nil, false, err
			}
			return filtered, true, nil
		default:
			return val, !isLinuxString(val), nil
		}
	})
}
