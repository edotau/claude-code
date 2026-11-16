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
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// keybindingsGOOS picks defaultKeybindingsDest's platform branch; tests swap it to run every branch on any host.
var keybindingsGOOS = runtime.GOOS

// defaultKeybindingsDest returns the local User keybindings.json; skip is true on a headless vscode-server box.
func defaultKeybindingsDest() (dest string, skip bool, note string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, "", err
	}
	switch keybindingsGOOS {
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

// syncKeybindings merges the embedded keybindings template into dest, a JSONC array. check mirrors syncSettings.
func syncKeybindings(template, dest string, check bool) (mergeResult, error) {
	rawTemplate := defaultKeybindingsTemplate
	if template != "" {
		b, err := os.ReadFile(template)
		if err != nil {
			return mergeResult{}, err
		}
		rawTemplate = b
	}
	var overlay []map[string]any
	if err := json.Unmarshal(textutil.StripJSONC(rawTemplate), &overlay); err != nil {
		return mergeResult{}, fmt.Errorf("keybindings template: %w", err)
	}

	if check {
		return mergeKeybindings(dest, overlay, true)
	}
	var result mergeResult
	err := withLock("vscode-keybindings.lock", dest, func() (mergeErr error) {
		result, mergeErr = mergeKeybindings(dest, overlay, false)
		return mergeErr
	})
	return result, err
}

func mergeKeybindings(dest string, overlay []map[string]any, check bool) (mergeResult, error) {
	var base []map[string]any
	rawBase, err := readBase(dest, "keybindings", &base)
	if err != nil {
		return mergeResult{}, err
	}
	merged := mergeKeybindingEntries(base, overlay)
	if reflect.DeepEqual(base, merged) {
		return mergeResult{}, nil
	}
	if check {
		return mergeResult{Changed: true}, nil
	}
	body, err := marshalKeybindings(merged, leadingComment(rawBase))
	if err != nil {
		return mergeResult{}, err
	}
	return writeGuarded(dest, "keybindings", rawBase, body)
}

// mergeKeybindingEntries replaces matching template entries in place and appends the rest, leaving user entries alone.
func mergeKeybindingEntries(base, overlay []map[string]any) []map[string]any {
	merged := make([]map[string]any, len(base))
	copy(merged, base)
	for _, entry := range overlay {
		if at := slices.IndexFunc(merged, func(existing map[string]any) bool { return keybindingMatch(existing, entry) }); at >= 0 {
			merged[at] = entry
			continue
		}
		merged = append(merged, entry)
	}
	return merged
}

// keybindingMatch keys on command + profileName, falling back to a deep args comparison.
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

// leadingComment captures the header comment before the array's opening bracket so a merge keeps it.
func leadingComment(raw []byte) string {
	rest := bytes.TrimLeft(raw, " \t\r\n")
	end := 0
	for {
		var n int
		switch {
		case bytes.HasPrefix(rest[end:], []byte("//")):
			n = bytes.IndexByte(rest[end:], '\n') + 1
			if n == 0 {
				n = len(rest) - end
			}
		case bytes.HasPrefix(rest[end:], []byte("/*")):
			i := bytes.Index(rest[end+2:], []byte("*/")) // after "/*", so "/*/" is not its own close
			if i < 0 {
				return ""
			}
			n = i + 4
		default:
			return strings.TrimSpace(string(rest[:end]))
		}
		end += n
		end += len(rest[end:]) - len(bytes.TrimLeft(rest[end:], " \t\r\n"))
	}
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
