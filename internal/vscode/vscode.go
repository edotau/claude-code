// Package vscode merges the tracked VS Code settings template into VS Code's Machine settings.json
// — the scope Settings Sync never syncs, so every box renders its own copy locally. Ported from
// claude-dbx's internal/settings/settingscmd/vscode.go.
package vscode

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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

// DefaultDest returns the platform's VS Code Machine settings path. WSL/Linux with a running
// vscode-server uses its Machine scope; macOS and Linux desktop fall back to the User scope, the
// closest equivalent outside a remote session.
func DefaultDest() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "settings.json"), nil
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Code", "User", "settings.json"), nil
	default:
		if info, statErr := os.Stat(filepath.Join(home, ".vscode-server")); statErr == nil && info.IsDir() {
			return filepath.Join(home, ".vscode-server", "data", "Machine", "settings.json"), nil
		}
		return filepath.Join(home, ".config", "Code", "User", "settings.json"), nil
	}
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
	rendered := strings.ReplaceAll(string(rawTemplate), "__CLAUDE_HOME__", claudeHome)
	rendered = strings.ReplaceAll(rendered, "__CLAUDE_BIN__", claudeBin)
	var overlay map[string]any
	if err := json.Unmarshal(stripJSONC([]byte(rendered)), &overlay); err != nil {
		return Result{}, fmt.Errorf("template: %w", err)
	}

	if check {
		// Read-only: --check/--dry-run must not drop a lock file beside the user's settings.
		return merge(dest, overlay, true)
	}
	// Read -> backup -> write is ONE critical section, serialized against a sibling invocation. The lock
	// lives in harness state, never beside VS Code's own file.
	var result Result
	lock := filepath.Join(paths.StateDir(), "vscode.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return Result{}, err
	}
	ran, err := paths.TryFileLock(lock, lockWait, func() error {
		r, mergeErr := merge(dest, overlay, false)
		result = r
		return mergeErr
	})
	if !ran {
		return Result{}, fmt.Errorf("vscode: could not acquire a lock on %s within %s", dest, lockWait)
	}
	return result, err
}

func merge(dest string, overlay map[string]any, check bool) (Result, error) {
	base := map[string]any{}
	rawBase, err := os.ReadFile(dest)
	switch {
	case err == nil && len(rawBase) > 0:
		if err := json.Unmarshal(stripJSONC(rawBase), &base); err != nil {
			return Result{}, fmt.Errorf("live settings: %w", err)
		}
	case err != nil && !os.IsNotExist(err):
		return Result{}, err
	}
	merged := cloneMap(base)
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
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Result{}, err
	}
	if BeforeWrite != nil {
		BeforeWrite(dest)
	}
	// VS Code holds no lock of ours, so re-read: this merge is only safe to write while the bytes it
	// merged are still the bytes on disk. A missing file reads as nil, matching a dest that never existed.
	fresh, err := os.ReadFile(dest)
	if err != nil && !os.IsNotExist(err) {
		return Result{}, err
	}
	if !bytes.Equal(fresh, rawBase) {
		return Result{}, fmt.Errorf("live settings changed under the merge (%s) — re-run to merge the newer file", dest)
	}
	backup := ""
	if len(rawBase) > 0 {
		backup = dest + ".bak-" + time.Now().Format("20060102-150405.000000000")
		if err := paths.AtomicWrite(backup, rawBase, 0o600); err != nil {
			return Result{}, err
		}
	}
	if err := paths.AtomicWrite(dest, body, 0o600); err != nil {
		return Result{Backup: backup}, err
	}
	return Result{Changed: true, Backup: backup}, nil
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
