package launch

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

// WindowsProfileDir is the WSL path of the Windows user profile (%USERPROFILE%), or "" when it can't
// be resolved (off WSL, cmd.exe unavailable, or the profile does not exist).
func WindowsProfileDir() string {
	if v := strings.TrimSpace(os.Getenv(windowsProfileEnv)); v != "" {
		return v
	}
	if !proc.OnWSL() {
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

// WindowsSettingsFile is the Windows Claude Code settings.json under the profile, or "" when the
// profile can't be resolved.
func WindowsSettingsFile() string {
	dir := WindowsProfileDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ".claude", "settings.json")
}

// InstallWindowsSettings renders the Windows subset of the live settings and merges it into the
// Windows settings.json (hooks only when the file exists), backing up first. A no-op off WSL, or with a one-line skip note when the
// Windows profile can't be resolved.
func InstallWindowsSettings(w io.Writer, dryRun bool) error {
	if !proc.OnWSL() && os.Getenv(windowsProfileEnv) == "" {
		return nil
	}
	file := WindowsSettingsFile()
	if file == "" {
		fmt.Fprintf(w, "settings (windows) skip  %s not resolved (cmd.exe unavailable?)\n", "%USERPROFILE%")
		return nil
	}
	rendered, err := settings.RenderLive()
	if err != nil {
		return err
	}
	winRendered, err := stripWindowsOnly(rendered)
	if err != nil {
		return err
	}
	existing, merged, sum, err := mergeWindowsSettings(file, winRendered)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "settings (windows) %s\n  hooks  %d harness refreshed + %d foreign kept; other keys yours\n",
		file, sum.HarnessHooks, sum.ForeignHooks)
	if string(merged) == string(existing) {
		fmt.Fprintln(w, "  unchanged")
		return nil
	}
	if dryRun {
		fmt.Fprintf(w, "  would write %d bytes (was %d); --dry-run wrote nothing\n", len(merged), len(existing))
		return nil
	}
	return updateWindowsSettings(w, file, winRendered)
}

func mergeWindowsSettings(file string, rendered []byte) ([]byte, []byte, settings.Summary, error) {
	existing, _, err := readStrippingBOM(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, settings.Summary{}, err
	}
	merged, sum, err := settings.Merge(existing, rendered)
	return existing, merged, sum, err
}

func updateWindowsSettings(w io.Writer, file string, winRendered []byte) error {
	// Re-merge from the bytes read under the file lock, so a Windows-side writer's concurrent edit survives.
	backup, err := settings.UpdateWithBackup(file, func(old []byte) ([]byte, error) {
		hadBOM := bytes.HasPrefix(old, bomBytes)
		m, _, err := settings.Merge(bytes.TrimPrefix(old, bomBytes), winRendered)
		if err != nil {
			return nil, err
		}
		if hadBOM {
			return append(append([]byte{}, bomBytes...), m...), nil
		}
		return m, nil
	})
	if backup != "" {
		fmt.Fprintf(w, "  backup %s\n", backup)
	}
	return err
}

// bomBytes is the UTF-8 byte-order mark; preserved on write only when the existing file carried one.
var bomBytes = []byte{0xEF, 0xBB, 0xBF}

// readStrippingBOM reads path and reports whether it carried a UTF-8 BOM, stripped from the returned bytes.
func readStrippingBOM(path string) (data []byte, hadBOM bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	if bytes.HasPrefix(raw, bomBytes) {
		return raw[len(bomBytes):], true, nil
	}
	return raw, false, nil
}

// CheckWindowsSettingsDrift compares the Windows settings.json against the Windows subset of the live
// render. ok is false when the Windows profile can't be resolved — the caller reports no line then.
func CheckWindowsSettingsDrift() (d SettingsDrift, ok bool, err error) {
	file := WindowsSettingsFile()
	if file == "" {
		return SettingsDrift{}, false, nil
	}
	rendered, err := settings.RenderLive()
	if err != nil {
		return SettingsDrift{File: file}, true, err
	}
	winRendered, err := stripWindowsOnly(rendered)
	if err != nil {
		return SettingsDrift{File: file}, true, err
	}
	d, err = checkSettingsDriftAt(file, winRendered)
	// Windows-side writers (foreign hooks, remote, a bash statusLine) own their keys; install keeps them, so not drift.
	d.Kept, d.Foreign = d.Foreign, nil
	return d, true, err
}

// checkSettingsDriftAt is CheckSettingsDriftWith generalized over the settings file.
func checkSettingsDriftAt(file string, rendered []byte) (SettingsDrift, error) {
	d := SettingsDrift{File: file}
	existing, _, rerr := readStrippingBOM(file) // install preserves a BOM; Diff must not see it
	switch {
	case rerr != nil && !errors.Is(rerr, fs.ErrNotExist):
		return d, rerr
	case rerr != nil:
		d.Differs = []string{"(missing)"}
	default:
		differs, foreign, derr := settings.Diff(existing, rendered)
		if derr != nil {
			if errors.Is(derr, settings.ErrInvalidJSON) {
				d.Invalid = fmt.Errorf("invalid JSON; restore %s", filepath.Base(newestBackup(file)))
				return d, nil
			}
			return d, derr
		}
		d.Differs, d.Foreign = differs, foreign
	}
	return d, nil
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
