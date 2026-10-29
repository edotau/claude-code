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

	"github.com/edotau/claude-code/internal/paths"
)

// DefaultKeybindingsDest returns the local client's User keybindings.json. Keybindings are UI-side —
// unlike settings there is no Machine scope — so a headless box with only vscode-server has nowhere
// to put them; skip is true and note explains why.
func DefaultKeybindingsDest() (dest string, skip bool, note string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "keybindings.json"), false, "", nil
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Code", "User", "keybindings.json"), false, "", nil
	default:
		clientDir := filepath.Join(home, ".config", "Code", "User")
		if _, statErr := os.Stat(clientDir); statErr != nil {
			if _, serverErr := os.Stat(filepath.Join(home, ".vscode-server")); serverErr == nil {
				return "", true, "no local VS Code client found (only ~/.vscode-server); keybindings are UI-side, skipping", nil
			}
		}
		return filepath.Join(clientDir, "keybindings.json"), false, "", nil
	}
}

// SyncKeybindings merges the embedded keybindings template into dest, a JSONC array. check mirrors Sync.
func SyncKeybindings(template, dest string, check bool) (Result, error) {
	rawTemplate := defaultKeybindingsTemplate
	if template != "" {
		b, err := os.ReadFile(template)
		if err != nil {
			return Result{}, err
		}
		rawTemplate = b
	}
	var overlay []map[string]any
	if err := json.Unmarshal(stripJSONC(rawTemplate), &overlay); err != nil {
		return Result{}, fmt.Errorf("keybindings template: %w", err)
	}

	if check {
		return mergeKeybindings(dest, overlay, true)
	}
	var result Result
	lock := filepath.Join(paths.StateDir(), "vscode-keybindings.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return Result{}, err
	}
	ran, err := paths.TryFileLock(lock, lockWait, func() error {
		r, mergeErr := mergeKeybindings(dest, overlay, false)
		result = r
		return mergeErr
	})
	if !ran {
		return Result{}, fmt.Errorf("vscode: could not acquire a lock on %s within %s", dest, lockWait)
	}
	return result, err
}

func mergeKeybindings(dest string, overlay []map[string]any, check bool) (Result, error) {
	var base []map[string]any
	rawBase, err := os.ReadFile(dest)
	switch {
	case err == nil && len(rawBase) > 0:
		if err := json.Unmarshal(stripJSONC(rawBase), &base); err != nil {
			return Result{}, fmt.Errorf("live keybindings: %w", err)
		}
	case err != nil && !os.IsNotExist(err):
		return Result{}, err
	}
	merged := mergeKeybindingEntries(base, overlay)
	if reflect.DeepEqual(base, merged) {
		return Result{}, nil
	}
	if check {
		return Result{Changed: true}, nil
	}
	body, err := marshalKeybindings(merged, leadingComment(rawBase))
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Result{}, err
	}
	if BeforeWrite != nil {
		BeforeWrite(dest)
	}
	// Same re-read guard as settings: only safe to write while the bytes merged are still on disk.
	fresh, err := os.ReadFile(dest)
	if err != nil && !os.IsNotExist(err) {
		return Result{}, err
	}
	if !bytes.Equal(fresh, rawBase) {
		return Result{}, fmt.Errorf("live keybindings changed under the merge (%s) — re-run to merge the newer file", dest)
	}
	backup := ""
	if len(rawBase) > 0 {
		var err error
		if backup, err = paths.Backup(dest, rawBase); err != nil {
			return Result{}, err
		}
	}
	if err := paths.AtomicWrite(dest, body, 0o600); err != nil {
		return Result{Backup: backup}, err
	}
	return Result{Changed: true, Backup: backup}, nil
}

// mergeKeybindingEntries replaces a matching template entry in place (by command + profile) and
// appends the rest, so user entries are never reordered or touched.
func mergeKeybindingEntries(base, overlay []map[string]any) []map[string]any {
	merged := make([]map[string]any, len(base))
	copy(merged, base)
	for _, entry := range overlay {
		replacedAt := -1
		for i, existing := range merged {
			if keybindingMatch(existing, entry) {
				replacedAt = i
				break
			}
		}
		if replacedAt >= 0 {
			merged[replacedAt] = entry
			continue
		}
		merged = append(merged, entry)
	}
	return merged
}

// keybindingMatch identifies a template entry by command + the profile it opens, falling back to a
// deep comparison of args when there is no profileName to key on.
func keybindingMatch(a, b map[string]any) bool {
	if a["command"] != b["command"] {
		return false
	}
	am, _ := a["args"].(map[string]any)
	bm, _ := b["args"].(map[string]any)
	if am != nil && bm != nil {
		if profile, ok := bm["profileName"]; ok {
			return am["profileName"] == profile
		}
	}
	return reflect.DeepEqual(a["args"], b["args"])
}

// leadingComment captures a header comment (VS Code's "Place your key bindings…") preceding the
// array's opening bracket, so a merge does not drop it.
func leadingComment(raw []byte) string {
	idx := bytes.IndexByte(raw, '[')
	if idx <= 0 {
		return ""
	}
	head := strings.TrimSpace(string(raw[:idx]))
	if head == "" || (!strings.HasPrefix(head, "//") && !strings.HasPrefix(head, "/*")) {
		return ""
	}
	return head
}

func marshalKeybindings(entries []map[string]any, leading string) ([]byte, error) {
	arr, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, err
	}
	var body []byte
	if leading != "" {
		body = append([]byte(leading+"\n"), arr...)
	} else {
		body = arr
	}
	return append(body, '\n'), nil
}

// defaultKeybindingsTemplate is the tracked first-party keybindings template, embedded for a bare install.
//
//go:embed keybindings.template.json
var defaultKeybindingsTemplate []byte
