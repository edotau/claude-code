package setup

import (
	"bytes"
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

// filterObject decodes a JSON object preserving key order, keeps the entries keep accepts (optionally
// transforming the value), and remarshals indented like the rest of the settings package.
func filterObject(raw []byte, keep func(key string, val json.RawMessage) (json.RawMessage, bool, error)) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if !objectStart(dec) {
		return nil, errors.New("not a JSON object")
	}
	var buf bytes.Buffer
	buf.WriteString("{")
	first := true
	for dec.More() {
		key, val, err := decodeObjectEntry(dec)
		if err != nil {
			return nil, err
		}
		newVal, ok, err := keep(key, val)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if !first {
			buf.WriteString(",")
		}
		first = false
		if err := writeObjectEntry(&buf, key, newVal); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if !first {
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

func objectStart(dec *json.Decoder) bool {
	tok, err := dec.Token()
	return err == nil && tok == json.Delim('{')
}

func decodeObjectEntry(dec *json.Decoder) (string, json.RawMessage, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", nil, err
	}
	key, _ := tok.(string)
	var val json.RawMessage
	err = dec.Decode(&val)
	return key, val, err
}

func writeObjectEntry(buf *bytes.Buffer, key string, val json.RawMessage) error {
	kb, err := json.Marshal(key)
	if err != nil {
		return err
	}
	buf.WriteString("\n  ")
	buf.Write(kb)
	buf.WriteString(": ")
	return json.Indent(buf, bytes.TrimSpace(val), "  ", "  ")
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
	return filterObject(rendered, func(key string, val json.RawMessage) (json.RawMessage, bool, error) {
		switch {
		case slices.Contains(windowsOnlyTopKeys, key):
			return nil, false, nil
		case key == "env":
			filtered, err := filterObject(val, func(_ string, v json.RawMessage) (json.RawMessage, bool, error) {
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
