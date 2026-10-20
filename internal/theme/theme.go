// Package theme applies the tracked theme/ directory: one JSON file per tool, merged over that tool's
// own config so a fresh box (or a clobbered panel) gets the look back with one verb.
package theme

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vscode"
)

var beforeWrite func(dest string)

// Dir is the tracked theme directory.
func Dir() string { return filepath.Join(paths.ConfigDir(), "theme") }

// Target is one tool's theme file and the config it merges into.
type Target struct {
	Name string // file stem under theme/
	Dest string // the tool's own config file
}

// Targets maps each theme/<name>.json to its destination; VS Code's path is platform-specific.
func Targets(home string) []Target {
	vs, _ := vscode.DefaultDest()
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	return []Target{
		{"vscode", vs},
		{"statusline", filepath.Join(paths.ConfigDir(), "statusline", "config.json")},
		{"opencode", filepath.Join(cfg, "opencode", "tui.json")},
		{"copilot", filepath.Join(home, ".copilot", "settings.json")},
		{"gemini", filepath.Join(home, ".gemini", "settings.json")},
	}
}

// Outcome is one target's result.
type Outcome struct {
	Name, Dest string
	Changed    bool
	Skipped    string // reason nothing was done (no theme file, no destination)
	Backup     string
}

// Apply merges every present theme/<name>.json into its destination. dryRun reports without writing.
// Keys the theme file omits are kept; a key it carries wins. Maps merge recursively.
func Apply(dir, home string, dryRun bool) ([]Outcome, error) {
	var out []Outcome
	for _, t := range Targets(home) {
		o := Outcome{Name: t.Name, Dest: t.Dest}
		src := filepath.Join(dir, t.Name+".json")
		overlay, err := readJSON(src)
		switch {
		case os.IsNotExist(err):
			o.Skipped = "no " + filepath.Base(src)
			out = append(out, o)
			continue
		case err != nil:
			return out, fmt.Errorf("%s: %w", src, err)
		case t.Dest == "":
			o.Skipped = "no destination on this platform"
			out = append(out, o)
			continue
		}
		o.Changed, o.Backup, err = mergeFile(t.Dest, overlay, dryRun)
		if err != nil {
			return out, fmt.Errorf("%s → %s: %w", t.Name, t.Dest, err)
		}
		out = append(out, o)
	}
	return out, nil
}

func readJSON(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(vscode.StripJSONC(b), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// mergeFile overlays onto dest's JSON (JSONC tolerated on read); unchanged means no write, no backup.
func mergeFile(dest string, overlay map[string]any, dryRun bool) (changed bool, backup string, err error) {
	base := map[string]any{}
	raw, err := os.ReadFile(dest)
	if err != nil && !os.IsNotExist(err) {
		return false, "", err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(vscode.StripJSONC(raw), &base); err != nil {
			return false, "", fmt.Errorf("existing file: %w", err)
		}
	}
	merged := deepMerge(base, overlay)
	if reflect.DeepEqual(base, merged) {
		return false, "", nil
	}
	if dryRun {
		return true, "", nil
	}
	body, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return false, "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, "", err
	}
	if beforeWrite != nil {
		beforeWrite(dest)
	}
	fresh, err := os.ReadFile(dest)
	if err != nil && !os.IsNotExist(err) {
		return false, "", err
	}
	if !bytes.Equal(fresh, raw) {
		return false, "", fmt.Errorf("destination changed under the merge (%s) — re-run to merge the newer file", dest)
	}
	if len(raw) > 0 {
		backup = dest + ".bak-" + time.Now().Format("20060102-150405")
		if err := paths.AtomicWrite(backup, raw, 0o600); err != nil {
			return false, "", err
		}
	}
	return true, backup, paths.AtomicWrite(dest, append(body, '\n'), 0o600)
}

// deepMerge returns a new map: base with overlay applied, nested maps merged, everything else replaced.
func deepMerge(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	maps.Copy(out, base)
	for k, v := range overlay {
		if ov, ok := v.(map[string]any); ok {
			if bv, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(bv, ov)
				continue
			}
		}
		out[k] = v
	}
	return out
}
