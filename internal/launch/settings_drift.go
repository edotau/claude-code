package launch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
)

// SettingsDrift holds every way the live settings.json (and its neighbors) can have drifted.
type SettingsDrift struct {
	File     string
	Invalid  error
	Differs  []string
	Foreign  []string
	LocalDup []string
	Nested   string
}

// Drifted is true when settings.json is invalid, missing, differs, holds a foreign key, or a nested config exists.
func (s SettingsDrift) Drifted() bool {
	return s.Invalid != nil || len(s.Differs) > 0 || len(s.Foreign) > 0 || s.Nested != ""
}

// Lines renders one <=80-col "settings: " line per finding; LocalDup is informational, not drift.
func (s SettingsDrift) Lines() []string {
	var lines []string
	switch {
	case s.Invalid != nil:
		lines = append(lines, "settings: "+s.Invalid.Error())
	case len(s.Differs) == 1 && s.Differs[0] == "(missing)":
		lines = append(lines, "settings: "+truncate80(s.File)+" is missing (run claude-code install)")
	default:
		for _, k := range s.Differs {
			lines = append(lines, "settings: "+truncate80(k)+" differs from the template")
		}
	}
	for _, k := range s.Foreign {
		lines = append(lines, "settings: "+truncate80(k)+" is a foreign key not in the template")
	}
	for _, l := range s.LocalDup {
		lines = append(lines, "settings: "+l)
	}
	if s.Nested != "" {
		lines = append(lines, "settings: nested config: "+truncate80(s.Nested))
	}
	return lines
}

// truncate80 shortens a path or key to 80 runes with a trailing "…", so the enclosing line stays <=80 cols.
func truncate80(s string) string {
	r := []rune(s)
	if len(r) <= 80 {
		return s
	}
	return string(r[:79]) + "…"
}

// CheckSettingsDrift renders the live settings and delegates to CheckSettingsDriftWith.
func CheckSettingsDrift() (SettingsDrift, error) {
	rendered, err := settings.RenderLive()
	if err != nil {
		return SettingsDrift{File: SettingsFile()}, err
	}
	return CheckSettingsDriftWith(rendered)
}

// CheckSettingsDriftWith compares the live settings.json against an already-rendered template, flags
// settings.local.json entries that only repeat it, and reports a nested <config dir>/.claude.
func CheckSettingsDriftWith(rendered []byte) (SettingsDrift, error) {
	file := SettingsFile()
	d := SettingsDrift{File: file}
	existing, rerr := os.ReadFile(file)
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
				break
			}
			return d, derr
		}
		d.Differs, d.Foreign = differs, foreign
	}
	d.LocalDup = localDuplicateLines(rendered)
	if nested := clean.NestedConfig(); nested.Dir != "" && !nested.OnlyWorktrees() {
		d.Nested = nestedDetail(nested)
	}
	return d, nil
}

// localDuplicateLines reports settings.local.json keys whose value only repeats the rendered template.
func localDuplicateLines(rendered []byte) []string {
	local, lerr := os.ReadFile(filepath.Join(paths.ConfigDir(), "settings.local.json"))
	if lerr != nil {
		return nil
	}
	dups, derr := settings.LocalDuplicates(local, rendered)
	if derr != nil {
		return nil
	}
	lines := make([]string, len(dups))
	for i, k := range dups {
		lines[i] = fmt.Sprintf("settings.local.json %s repeats the template (only read in $HOME)", k)
	}
	return lines
}

// newestBackup names the latest <file>.bak-* to restore from, or says none was found.
func newestBackup(file string) string {
	matches, _ := filepath.Glob(file + ".bak-*")
	if len(matches) == 0 {
		return "no backup found"
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}
