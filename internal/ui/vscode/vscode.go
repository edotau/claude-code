// Package vscode merges the tracked VS Code settings template into VS Code's Machine settings.json
// — the scope Settings Sync never syncs, so every box renders its own copy locally.
package vscode

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// lockWait bounds the wait for a sibling invocation; past the budget the merge errors rather than hang.
const lockWait = 5 * time.Second

// beforeWrite is a test seam: it runs inside the lock, immediately before the staleness re-check, so
// a foreign save landing in that window can be observed deterministically.
var beforeWrite func(dest string)

// mergeResult is what one merge attempt produced.
type mergeResult struct {
	Changed bool
	Backup  string
}

// syncSettings renders template with __CLAUDE_HOME__/__CLAUDE_BIN__ expanded, then merges it into dest.
// check=true never writes — --check (drift probe) and --dry-run (preview) both pass it; the caller
// tells them apart by message and exit code.
func syncSettings(template, dest, claudeHome, claudeBin string, check bool) (mergeResult, error) {
	rawTemplate := defaultTemplate // the embedded template ships with the binary; --template overrides it
	if template != "" {
		b, err := os.ReadFile(template)
		if err != nil {
			return mergeResult{}, err
		}
		rawTemplate = b
	}
	rendered := strings.ReplaceAll(string(rawTemplate), "__CLAUDE_HOME__", jsonEscape(claudeHome))
	rendered = strings.ReplaceAll(rendered, "__CLAUDE_BIN__", jsonEscape(claudeBin))
	var overlay map[string]any
	if err := json.Unmarshal(textutil.StripJSONC([]byte(rendered)), &overlay); err != nil {
		return mergeResult{}, fmt.Errorf("template: %w", err)
	}

	if check {
		// Read-only: --check/--dry-run must not drop a lock file beside the user's settings.
		return merge(dest, overlay, claudeBin+"/claude", true)
	}
	// Read -> backup -> write is ONE critical section, serialized against a sibling invocation. The lock
	// lives in harness state, never beside VS Code's own file.
	var result mergeResult
	err := withLock("vscode.lock", dest, func() (mergeErr error) {
		result, mergeErr = merge(dest, overlay, claudeBin+"/claude", false)
		return mergeErr
	})
	return result, err
}

// withLock runs fn under the named harness-state lock; a lock that cannot be opened or won in time is an error.
func withLock(name, dest string, fn func() error) error {
	lock := filepath.Join(paths.StateDir(), name)
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return err
	}
	ran, err := paths.TryFileLock(lock, lockWait, fn)
	if !ran && err != nil {
		return fmt.Errorf("vscode: lock %s: %w", lock, err)
	}
	if !ran {
		return fmt.Errorf("vscode: could not acquire a lock on %s within %s", dest, lockWait)
	}
	return err
}

func merge(dest string, overlay map[string]any, claudeWrapper string, check bool) (mergeResult, error) {
	base := map[string]any{}
	rawBase, err := readBase(dest, "settings", &base)
	if err != nil {
		return mergeResult{}, err
	}
	stripped := maps.Clone(base)
	if base["claudeCode.claudeProcessWrapper"] == claudeWrapper {
		delete(stripped, "claudeCode.claudeProcessWrapper")
	}
	merged := theme.DeepMerge(stripped, overlay)
	if reflect.DeepEqual(base, merged) {
		return mergeResult{}, nil
	}
	if check {
		return mergeResult{Changed: true}, nil
	}
	body, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return mergeResult{}, err
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
func writeGuarded(dest, label string, rawBase, body []byte) (mergeResult, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return mergeResult{}, err
	}
	if beforeWrite != nil {
		beforeWrite(dest)
	}
	// VS Code takes no lock of ours: write only while the merged bytes are still on disk (missing reads as nil).
	backup, err := paths.WriteIfUnchanged(dest, rawBase, body, 0o600, label)
	if err != nil {
		return mergeResult{Backup: backup}, err
	}
	return mergeResult{Changed: true, Backup: backup}, nil
}

// defaultTemplate is the tracked first-party VS Code settings template, embedded so a bare install has it.
//
//go:embed machine-settings.template.json
var defaultTemplate []byte

// syncOptions are `theme vscode`'s paths; an empty field resolves to the platform default.
type syncOptions struct {
	Template, Dest, ExtensionsDir, KeybindingsTemplate, KeybindingsDest string
	Check                                                               bool // never write (--check and --dry-run)
}

// outcome is one sync run's results: settings template, keybindings and extension link.
type outcome struct {
	DestPath   string
	Result     mergeResult
	KbDestPath string
	Skip       bool // keybindings were not synced; Note says why
	Note       string
	KbResult   mergeResult
	ExtResult  extensionResult
}

// syncAll merges the settings and keybindings templates and links the Agents extension.
func syncAll(o syncOptions) (outcome, error) {
	out := outcome{DestPath: o.Dest}
	if o.Dest == "" {
		p, err := paths.VSCodeSettings()
		if err != nil {
			return out, err
		}
		out.DestPath = p
	}
	var err error
	if out.Result, err = syncSettings(o.Template, out.DestPath, paths.ConfigDir(), paths.BinDir(), o.Check); err != nil {
		return out, err
	}
	if out.KbDestPath, out.Skip, out.Note, err = resolveKeybindingsDest(o.KeybindingsDest, o.Dest, out.DestPath); err != nil {
		return out, err
	}
	if !out.Skip {
		if out.KbResult, err = syncKeybindings(o.KeybindingsTemplate, out.KbDestPath, o.Check); err != nil {
			return out, err
		}
	}
	extDir := o.ExtensionsDir
	if extDir == "" {
		if extDir, err = defaultExtensionsDir(); err != nil {
			return out, err
		}
	}
	out.ExtResult, err = linkExtension(paths.ConfigDir(), extDir, o.Check)
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
		return defaultKeybindingsDest()
	}
}
