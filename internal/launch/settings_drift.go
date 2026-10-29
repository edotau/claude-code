package launch

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

// maxLineRunes is the CLI text width cap for one drift line.
const maxLineRunes = 80

// SettingsDrift holds every way the live settings.json (and its neighbors) can have drifted.
type SettingsDrift struct {
	File     string
	Invalid  error
	Differs  []string
	Foreign  []string
	Kept     []string // foreign keys reported but not counted as drift (the Windows file)
	LocalDup []string
	Nested   string
	Note     string // informational (e.g. drop list unavailable); not drift
}

// Drifted is true when settings.json is invalid, missing, differs, holds a foreign key, or a nested config exists.
func (s SettingsDrift) Drifted() bool {
	return s.Invalid != nil || len(s.Differs) > 0 || len(s.Foreign) > 0 || s.Nested != ""
}

// Lines renders one <=80-col "settings: " line per finding; LocalDup is informational, not drift.
func (s SettingsDrift) Lines() []string { return s.LinesLabeled("settings") }

// LinesLabeled is Lines with a different line prefix, e.g. "settings (windows)".
func (s SettingsDrift) LinesLabeled(label string) []string {
	var lines []string
	add := func(l string) { lines = append(lines, textutil.Truncate(l, maxLineRunes)) }
	// addKey shortens the key, not the line, so the verb saying what is wrong always fits
	addKey := func(k, verb string) {
		room := maxLineRunes - utf8.RuneCountInString(label+": "+verb)
		add(label + ": " + textutil.Truncate(k, max(room, 8)) + verb)
	}
	switch {
	case s.Invalid != nil:
		add(label + ": " + s.Invalid.Error())
	case len(s.Differs) == 1 && s.Differs[0] == "(missing)":
		add(label + ": " + s.File + " is missing (run claude-code install)")
	default:
		for _, k := range s.Differs {
			addKey(k, " differs from the template")
		}
	}
	for _, k := range s.Foreign {
		addKey(k, " is a foreign key not in the template")
	}
	for _, k := range s.Kept {
		addKey(k, " is kept (not in the template)")
	}
	for _, l := range s.LocalDup {
		add(label + ": " + l)
	}
	if s.Nested != "" {
		add(label + ": nested config: " + s.Nested)
	}
	if s.Note != "" {
		add(label + ": " + s.Note)
	}
	return lines
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
	d, err := checkSettingsDriftAt(file, filepath.Join(paths.ConfigDir(), "settings.local.json"), rendered, " (only read in $HOME)")
	if err != nil {
		return d, err
	}
	d.restoreDiff(rendered)
	dropped, derr := settings.LoadDropped()
	if derr != nil { // match install: warn and continue with an empty list
		d.Note = "drop list unavailable: " + derr.Error()
	}
	d.applyDropped(dropped, rendered)
	if nested := clean.NestedConfig(); nested.Dir != "" && !nested.OnlyWorktrees() {
		d.Nested = nestedDetail(nested)
	}
	return d, nil
}

// applyDropped moves live drop-list keys from Foreign to a Differs line; install removes them.
func (s *SettingsDrift) applyDropped(dropped []string, rendered []byte) {
	if len(dropped) == 0 || s.Invalid != nil {
		return
	}
	live, _, err := readStrippingBOM(s.File)
	if err != nil {
		return
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(live, &top) != nil {
		return
	}
	var tpl map[string]json.RawMessage
	_ = json.Unmarshal(rendered, &tpl)
	for _, k := range dropped {
		if _, ok := top[k]; !ok {
			continue
		}
		if _, owned := tpl[k]; owned {
			continue
		}
		s.Foreign = slices.DeleteFunc(s.Foreign, func(f string) bool { return f == k })
		s.Differs = append(s.Differs, k+" is dropped (run claude-code install)")
	}
}

// newestBackup names the latest cache-mirrored backup to restore from, or says none was found.
func newestBackup(file string) string {
	sample := paths.BackupPath(file, time.Now())
	prefix := sample[:strings.LastIndex(sample, ".bak-")+len(".bak-")]
	matches, _ := filepath.Glob(prefix + "*")
	if len(matches) == 0 {
		return "no backup found"
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

// Broken is the part of drift that stays a failure: invalid JSON, a missing file, or a nested config.
func (s SettingsDrift) Broken() bool {
	return s.Invalid != nil || s.Nested != "" || (len(s.Differs) == 1 && s.Differs[0] == "(missing)")
}

// DoctorSettingsDrift is the doctor row: ok is false only for Broken drift; keys differing from the
// defaults are information.
func DoctorSettingsDrift() (ok bool, detail string) {
	d, err := CheckSettingsDrift()
	if err != nil {
		return false, err.Error()
	}
	if d.Broken() {
		return false, settingsDriftDetail(d, nil)
	}
	keys, err := settings.DefaultsDrift()
	if err != nil {
		return false, err.Error()
	}
	d.Differs, d.Foreign = nil, nil
	var parts []string
	if len(keys) > 0 {
		parts = append(parts, fmt.Sprintf("%d keys differ from your defaults — claude-code configure --restore", len(keys)))
	}
	if rest := settingsDriftDetail(d, nil); rest != "clean" {
		parts = append(parts, rest)
	}
	if len(parts) == 0 {
		return true, "clean"
	}
	return true, strings.Join(parts, "; ")
}

// restoreDiff replaces the merge-based findings with the keys --restore would actually change or remove.
func (s *SettingsDrift) restoreDiff(rendered []byte) {
	if s.Invalid != nil || (len(s.Differs) == 1 && s.Differs[0] == "(missing)") {
		return
	}
	live, _, err := readStrippingBOM(s.File)
	if err != nil {
		return
	}
	keys, err := settings.DiffRendered(live, rendered)
	if err != nil {
		return
	}
	var have, want map[string]json.RawMessage
	_ = json.Unmarshal(live, &have)
	_ = json.Unmarshal(rendered, &want)
	s.Differs, s.Foreign = nil, nil
	for _, k := range keys {
		_, inLive := have[k]
		if _, inRender := want[k]; inLive && !inRender {
			s.Foreign = append(s.Foreign, k)
		} else {
			s.Differs = append(s.Differs, k)
		}
	}
}
