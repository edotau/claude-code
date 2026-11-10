// Package vscode merges the tracked VS Code settings template into VS Code's Machine settings.json
// — the scope Settings Sync never syncs, so every box renders its own copy locally.
package vscode

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// lockWait bounds the wait for a sibling invocation; past the budget the merge errors rather than hang.
const lockWait = 5 * time.Second

// BeforeWrite is a test seam: it runs inside the lock, immediately before the staleness re-check, so
// a foreign save landing in that window can be observed deterministically.
var BeforeWrite func(dest string)

// Result is what one merge attempt produced.
type Result struct {
	Changed bool
	Backup  string
}

// Sync renders template with __CLAUDE_HOME__/__CLAUDE_BIN__ expanded, then merges it into dest.
// check=true never writes — --check (drift probe) and --dry-run (preview) both pass it; the caller
// tells them apart by message and exit code.
func Sync(template, dest, claudeHome, claudeBin string, check bool) (Result, error) {
	rawTemplate := defaultTemplate // the embedded template ships with the binary; --template overrides it
	if template != "" {
		b, err := os.ReadFile(template)
		if err != nil {
			return Result{}, err
		}
		rawTemplate = b
	}
	rendered := strings.ReplaceAll(string(rawTemplate), "__CLAUDE_HOME__", jsonEscape(claudeHome))
	rendered = strings.ReplaceAll(rendered, "__CLAUDE_BIN__", jsonEscape(claudeBin))
	var overlay map[string]any
	if err := json.Unmarshal(textutil.StripJSONC([]byte(rendered)), &overlay); err != nil {
		return Result{}, fmt.Errorf("template: %w", err)
	}

	if check {
		// Read-only: --check/--dry-run must not drop a lock file beside the user's settings.
		return merge(dest, overlay, claudeBin+"/claude", true)
	}
	// Read -> backup -> write is ONE critical section, serialized against a sibling invocation. The lock
	// lives in harness state, never beside VS Code's own file.
	var result Result
	lock := filepath.Join(paths.StateDir(), "vscode.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return Result{}, err
	}
	ran, err := paths.TryFileLock(lock, lockWait, func() error {
		r, mergeErr := merge(dest, overlay, claudeBin+"/claude", false)
		result = r
		return mergeErr
	})
	if !ran && err != nil {
		return Result{}, fmt.Errorf("vscode: lock %s: %w", lock, err)
	}
	if !ran {
		return Result{}, fmt.Errorf("vscode: could not acquire a lock on %s within %s", dest, lockWait)
	}
	return result, err
}

func merge(dest string, overlay map[string]any, claudeWrapper string, check bool) (Result, error) {
	base := map[string]any{}
	rawBase, err := readBase(dest, "settings", &base)
	if err != nil {
		return Result{}, err
	}
	merged := cloneMap(base)
	if base["claudeCode.claudeProcessWrapper"] == claudeWrapper {
		delete(merged, "claudeCode.claudeProcessWrapper")
	}
	mergeInto(merged, overlay)
	if reflect.DeepEqual(base, merged) {
		return Result{}, nil
	}
	if check {
		return Result{Changed: true}, nil
	}
	body, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return Result{}, err
	}
	body = append(body, '\n')
	return writeGuarded(dest, "settings", rawBase, body)
}

// jsonEscape returns s escaped for use inside a JSON string literal (a path may hold quotes or backslashes).
func jsonEscape(s string) string {
	b, _ := json.Marshal(s) // marshaling a string cannot fail
	return string(b[1 : len(b)-1])
}

// readBase loads dest's JSONC into into (a missing or empty file leaves it untouched) and returns the raw bytes.
func readBase(dest, label string, into any) ([]byte, error) {
	rawBase, err := os.ReadFile(dest)
	switch {
	case err == nil && len(rawBase) > 0:
		if err := json.Unmarshal(textutil.StripJSONC(rawBase), into); err != nil {
			return nil, fmt.Errorf("live %s: %w", label, err)
		}
	case err != nil && !os.IsNotExist(err):
		return nil, err
	}
	return rawBase, nil
}

// writeGuarded backs dest up and atomically replaces it with body, but only while rawBase is still on disk.
func writeGuarded(dest, label string, rawBase, body []byte) (Result, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Result{}, err
	}
	if BeforeWrite != nil {
		BeforeWrite(dest)
	}
	// VS Code takes no lock of ours: write only while the merged bytes are still on disk (missing reads as nil).
	fresh, err := checkUnchanged(dest, label, rawBase)
	if err != nil {
		return Result{}, err
	}
	backup := ""
	if len(fresh) > 0 {
		if backup, err = paths.Backup(dest, fresh); err != nil {
			return Result{}, err
		}
	}
	// The backup write widens the window a VS Code save can land in, so re-check right before the replace.
	if _, err := checkUnchanged(dest, label, rawBase); err != nil {
		return Result{}, err
	}
	if err := paths.AtomicWrite(dest, body, 0o600); err != nil {
		return Result{Backup: backup}, err
	}
	return Result{Changed: true, Backup: backup}, nil
}

// readLive is os.ReadFile for checkUnchanged; tests swap it to land a VS Code save between the two checks.
var readLive = os.ReadFile

// checkUnchanged returns dest's current bytes, or an error when they no longer match rawBase.
func checkUnchanged(dest, label string, rawBase []byte) ([]byte, error) {
	fresh, err := readLive(dest)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if !bytes.Equal(fresh, rawBase) {
		return nil, fmt.Errorf("live %s changed under the merge (%s) — re-run to merge the newer file", label, dest)
	}
	return fresh, nil
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		if nested, ok := value.(map[string]any); ok {
			out[key] = cloneMap(nested)
		} else {
			out[key] = value
		}
	}
	return out
}

func mergeInto(base, overlay map[string]any) {
	for key, value := range overlay {
		if incoming, ok := value.(map[string]any); ok {
			if current, ok := base[key].(map[string]any); ok {
				mergeInto(current, incoming)
				continue
			}
			base[key] = cloneMap(incoming)
			continue
		}
		base[key] = value
	}
}

// defaultTemplate is the tracked first-party VS Code settings template, embedded so a bare install has it.
//
//go:embed machine-settings.template.json
var defaultTemplate []byte

// SyncOptions are `theme vscode`'s paths; an empty field resolves to the platform default.
type SyncOptions struct {
	Template, Dest, ExtensionsDir, KeybindingsTemplate, KeybindingsDest string
	Check                                                               bool // never write (--check and --dry-run)
}

// Outcome is one sync run's results: settings template, keybindings and extension link.
type Outcome struct {
	DestPath   string
	Result     Result
	KbDestPath string
	Skip       bool // keybindings were not synced; Note says why
	Note       string
	KbResult   Result
	ExtResult  ExtensionResult
}

// SyncAll merges the settings and keybindings templates and links the Agents extension.
func SyncAll(o SyncOptions) (Outcome, error) {
	out := Outcome{DestPath: o.Dest}
	if o.Dest == "" {
		p, err := paths.VSCodeSettings()
		if err != nil {
			return out, err
		}
		out.DestPath = p
	}
	var err error
	if out.Result, err = Sync(o.Template, out.DestPath, paths.ConfigDir(), paths.BinDir(), o.Check); err != nil {
		return out, err
	}
	if out.KbDestPath, out.Skip, out.Note, err = resolveKeybindingsDest(o.KeybindingsDest, o.Dest, out.DestPath); err != nil {
		return out, err
	}
	if !out.Skip {
		if out.KbResult, err = SyncKeybindings(o.KeybindingsTemplate, out.KbDestPath, o.Check); err != nil {
			return out, err
		}
	}
	extDir := o.ExtensionsDir
	if extDir == "" {
		if extDir, err = DefaultExtensionsDir(); err != nil {
			return out, err
		}
	}
	out.ExtResult, err = LinkExtension(paths.ConfigDir(), extDir, o.Check)
	return out, err
}

// resolveKeybindingsDest: explicit kbDest wins outright; an explicit dest with no kbDest uses the same
// directory as the settings file, never the platform's vscode-server Machine guess for an unrelated dest.
func resolveKeybindingsDest(kbDest, dest, destPath string) (path string, skip bool, note string, err error) {
	switch {
	case kbDest != "":
		return kbDest, false, "", nil
	case dest != "":
		return filepath.Join(filepath.Dir(destPath), "keybindings.json"), false, "", nil
	default:
		return DefaultKeybindingsDest()
	}
}
