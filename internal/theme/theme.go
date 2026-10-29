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

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vscode"
)

var beforeWrite func(dest string)

// Dir is the tracked theme directory.
func Dir() string { return filepath.Join(paths.ConfigDir(), "theme") }

// Target is one tool's theme file and the config it merges into.
type Target struct {
	Name string // display name for output
	Dest string // the tool's own config file
	Src  string // source file in theme/, defaults to Name+".json" if empty
}

// Targets maps each theme/<name>.json to its destination; VS Code's path is platform-specific.
func Targets(home string) []Target {
	vs, _ := vscode.DefaultDest()
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	return []Target{
		{"vscode", vs, ""},
		{"statusline", filepath.Join(paths.ConfigDir(), "state", "statusline", "config.json"), ""},
		{"opencode", filepath.Join(paths.StateDir(), "opencode", "tui.json"), ""},
		{"opencode-themes/gemini-plum", filepath.Join(cfg, "opencode", "themes", "gemini-plum.json"), ""},
		{"copilot", filepath.Join(home, ".copilot", "settings.json"), ""},
		{"gemini", filepath.Join(home, ".gemini", "settings.json"), ""},
		{"ghostty", filepath.Join(cfg, "ghostty", "config"), "ghostty.conf"},
	}
}

// Outcome is one target's result.
type Outcome struct {
	Name, Dest string
	Changed    bool
	Skipped    string // reason nothing was done (no theme file, no destination)
	Backup     string
}

// Apply merges every present theme/<name>.json (or .conf) into its destination. dryRun reports without writing.
// Keys the theme file omits are kept; a key it carries wins. Maps merge recursively for JSON; lines replace for .conf.
func Apply(dir, home string, dryRun bool) ([]Outcome, error) {
	var out []Outcome
	for _, t := range Targets(home) {
		o := Outcome{Name: t.Name, Dest: t.Dest}
		src := t.Src
		if src == "" {
			src = t.Name + ".json"
		}
		srcPath := filepath.Join(dir, src)
		merge, err := loadOverlay(srcPath)
		switch {
		case os.IsNotExist(err):
			o.Skipped = "no " + filepath.Base(srcPath)
			out = append(out, o)
			continue
		case err != nil:
			return out, fmt.Errorf("%s: %w", srcPath, err)
		case t.Dest == "":
			o.Skipped = "no destination on this platform"
			out = append(out, o)
			continue
		}
		o.Changed, o.Backup, err = merge(t.Dest, dryRun)
		if err != nil {
			return out, fmt.Errorf("%s → %s: %w", t.Name, t.Dest, err)
		}
		out = append(out, o)
	}
	return out, nil
}

type mergeFunc func(dest string, dryRun bool) (changed bool, backup string, err error)

// loadOverlay reads one theme file and returns the merge that applies it: line-wise for .conf, JSON otherwise.
func loadOverlay(path string) (mergeFunc, error) {
	if filepath.Ext(path) == ".conf" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return func(dest string, dryRun bool) (bool, string, error) { return confMergeFile(dest, raw, dryRun) }, nil
	}
	overlay, err := readJSON(path)
	if err != nil {
		return nil, err
	}
	return func(dest string, dryRun bool) (bool, string, error) { return mergeFile(dest, overlay, dryRun) }, nil
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

// writeMerged writes newContent with backup and concurrent-change check.
func writeMerged(dest string, oldContent, newContent []byte, dryRun bool, mode os.FileMode) (changed bool, backup string, err error) {
	if bytes.Equal(oldContent, newContent) {
		return false, "", nil
	}
	if dryRun {
		return true, "", nil
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
	if !bytes.Equal(fresh, oldContent) {
		return false, "", fmt.Errorf("destination changed under the merge (%s) — re-run to merge the newer file", dest)
	}
	if len(oldContent) > 0 {
		backup, err = paths.Backup(dest, oldContent)
		if err != nil {
			return false, "", err
		}
	}
	return true, backup, paths.AtomicWrite(dest, newContent, mode)
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
	body, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return false, "", err
	}
	return writeMerged(dest, raw, append(body, '\n'), dryRun, 0o600)
}

// confMergeFile merges overlay into dest for key=value config files.
func confMergeFile(dest string, overlay []byte, dryRun bool) (changed bool, backup string, err error) {
	raw, err := os.ReadFile(dest)
	if err != nil && !os.IsNotExist(err) {
		return false, "", err
	}
	return writeMerged(dest, raw, mergeGhostty(raw, overlay), dryRun, 0o644)
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
