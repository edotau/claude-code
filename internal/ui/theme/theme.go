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
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

var beforeWrite func(dest string)

// Dir is the tracked theme directory.
func Dir() string { return filepath.Join(paths.ConfigDir(), "theme") }

// target is one tool's theme file and the config it merges into.
type target struct {
	Name string // display name for output
	Dest string // the tool's own config file
	Src  string // source file in theme/, defaults to Name+".json" if empty
}

// targets maps each theme/<name>.json to its destination; VS Code's path is platform-specific.
func targets(home string) []target {
	vs, _ := paths.VSCodeSettings()
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	return []target{
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
func Apply(dir, home string, only []string, dryRun bool) ([]Outcome, error) {
	targets, err := selectTargets(targets(home), only)
	if err != nil {
		return nil, err
	}
	var out []Outcome
	for _, t := range targets {
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

// selectTargets keeps the named targets (a name also picks "<name>/…"); empty keeps all, an unknown name errors.
func selectTargets(all []target, only []string) ([]target, error) {
	if len(only) == 0 {
		return all, nil
	}
	var out []target
	for _, n := range only {
		hit := false
		for _, t := range all {
			if t.Name == n || strings.HasPrefix(t.Name, n+"/") {
				out, hit = append(out, t), true
			}
		}
		if !hit {
			names := make([]string, len(all))
			for i, t := range all {
				names[i] = t.Name
			}
			return nil, fmt.Errorf("unknown theme target %q (valid: %s)", n, strings.Join(names, ", "))
		}
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
	if err := json.Unmarshal(textutil.StripJSONC(b), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// writeMerged writes newContent under dest's lock, with backup and a concurrent-change check.
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
	err = paths.WithFileLock(dest, func() error {
		var werr error
		backup, werr = replaceIfUnchanged(dest, oldContent, newContent, mode)
		return werr
	})
	if err != nil {
		return false, "", err
	}
	return true, backup, nil
}

// replaceIfUnchanged backs up oldContent and swaps in newContent; the caller holds dest's lock.
func replaceIfUnchanged(dest string, oldContent, newContent []byte, mode os.FileMode) (string, error) {
	if beforeWrite != nil {
		beforeWrite(dest)
	}
	return paths.WriteIfUnchanged(dest, oldContent, newContent, mode, "theme file")
}

// mergeFile overlays onto dest's JSON (JSONC tolerated on read); unchanged means no write, no backup.
func mergeFile(dest string, overlay map[string]any, dryRun bool) (changed bool, backup string, err error) {
	base := map[string]any{}
	raw, err := os.ReadFile(dest)
	if err != nil && !os.IsNotExist(err) {
		return false, "", err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(textutil.StripJSONC(raw), &base); err != nil {
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

// mergeGhostty returns dest with each overlay key's lines replacing that key's dest lines at its first
// position; keys new to dest are appended in overlay order; overlay comments are dropped.
// Indexed palette overlays ("palette = N=#rrggbb") merge per index and keep unmentioned dest entries.
func mergeGhostty(dest, overlay []byte) []byte {
	set, order := overlayKeys(overlay)
	byIdx, indexed := paletteByIndex(set["palette"])
	var out [][]byte
	done := map[string]bool{}
	used := map[string]bool{}
	for _, line := range splitLines(dest) {
		key := parseKey(line)
		lines, ok := set[key]
		if indexed && key == "palette" {
			idx := paletteIndex(line)
			if r, hit := byIdx[idx]; hit && idx != "" {
				line, used[idx] = r, true
			}
			out = append(out, line)
			done[key] = true
			continue
		}
		switch {
		case key == "" || !ok:
			out = append(out, line)
		case !done[key]:
			out = append(out, lines...)
			done[key] = true
		}
	}
	for _, key := range order {
		switch {
		case indexed && key == "palette":
			for _, l := range set[key] {
				if !used[paletteIndex(l)] {
					out = append(out, l)
				}
			}
		case !done[key]:
			out = append(out, set[key]...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return append(bytes.Join(out, []byte("\n")), '\n')
}

// paletteIndex returns N of a "palette = N=color" line, or "" when the value has no index.
func paletteIndex(line []byte) string {
	_, val, _ := strings.Cut(string(line), "=")
	idx, _, ok := strings.Cut(val, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(idx)
}

// paletteByIndex maps index to line, and reports whether every line carries an index.
func paletteByIndex(lines [][]byte) (map[string][]byte, bool) {
	m := map[string][]byte{}
	for _, l := range lines {
		idx := paletteIndex(l)
		if idx == "" {
			return nil, false
		}
		m[idx] = l
	}
	return m, len(m) > 0
}

// overlayKeys groups the overlay's setting lines by key, with the keys in first-seen order.
func overlayKeys(overlay []byte) (set map[string][][]byte, order []string) {
	set = map[string][][]byte{}
	for _, line := range splitLines(overlay) {
		key := parseKey(line)
		if key == "" {
			continue
		}
		if _, ok := set[key]; !ok {
			order = append(order, key)
		}
		set[key] = append(set[key], line)
	}
	return set, order
}

// splitLines splits on newlines without the phantom empty line a trailing newline leaves.
func splitLines(b []byte) [][]byte {
	b = bytes.TrimRight(b, "\n")
	if len(b) == 0 {
		return nil
	}
	return bytes.Split(b, []byte("\n"))
}

// parseKey returns the key of a "key = value" line, or "" for blanks, comments and non-settings.
func parseKey(line []byte) string {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	key, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(key)
}
