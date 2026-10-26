package launch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
)

// freshConfigDir points CLAUDE_CONFIG_DIR at an empty temp dir, isolating SettingsFile()/paths.ConfigDir().
func freshConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

// TestCheckSettingsDriftClean: a freshly rendered settings.json (RenderLive, no hand edits) must not drift.
func TestCheckSettingsDriftClean(t *testing.T) {
	freshConfigDir(t)
	rendered, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SettingsFile(), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if d.Drifted() {
		t.Errorf("clean settings.json reports drift: %v", d.Lines())
	}
}

// TestCheckSettingsDriftMissingFile: no settings.json at all is still drift, reported as "(missing)".
func TestCheckSettingsDriftMissingFile(t *testing.T) {
	freshConfigDir(t)
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Drifted() {
		t.Fatal("a missing settings.json must report drift")
	}
	found := false
	for _, k := range d.Differs {
		if k == "(missing)" {
			found = true
		}
	}
	if !found {
		t.Errorf("Differs = %v, want it to contain %q", d.Differs, "(missing)")
	}
}

// TestCheckSettingsDriftInvalidJSON: broken JSON sets Invalid and names the newest .bak-* file to restore from.
func TestCheckSettingsDriftInvalidJSON(t *testing.T) {
	freshConfigDir(t)
	file := SettingsFile()
	backup := paths.BackupPath(file, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{not valid json,"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if d.Invalid == nil {
		t.Fatal("Invalid = nil, want an error for broken JSON")
	}
	if !d.Drifted() {
		t.Error("invalid JSON must report drift")
	}
	joined := strings.Join(d.Lines(), "\n")
	if !strings.Contains(joined, filepath.Base(backup)) {
		t.Errorf("Lines() = %v, want it to name the backup %q", d.Lines(), filepath.Base(backup))
	}
}

// TestCheckSettingsDriftForeignKey: a key the template never owned and settings.json alone added is Foreign.
func TestCheckSettingsDriftForeignKey(t *testing.T) {
	freshConfigDir(t)
	rendered, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimRight(strings.TrimSpace(string(rendered)), "}")
	withForeign := trimmed + `,"verbose":true}`
	if err := os.WriteFile(SettingsFile(), []byte(withForeign), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Drifted() {
		t.Fatal("a foreign key must report drift")
	}
	found := false
	for _, k := range d.Foreign {
		if k == "verbose" {
			found = true
		}
	}
	if !found {
		t.Errorf("Foreign = %v, want it to contain %q", d.Foreign, "verbose")
	}
}

// TestCheckSettingsDriftNested: a nested <config>/.claude directory is reported via Nested.
func TestCheckSettingsDriftNested(t *testing.T) {
	dir := freshConfigDir(t)
	rendered, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SettingsFile(), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if d.Nested == "" {
		t.Fatal("Nested = \"\", want the nested path reported")
	}
	if !d.Drifted() {
		t.Error("a nested .claude directory must report drift")
	}
}

// TestCheckSettingsDriftLocalDupNotDrift: settings.local.json repeating a rendered value is reported
// (LocalDup) but must not, by itself, flip Drifted() true — it is a redundancy note, not breakage.
func TestCheckSettingsDriftLocalDupNotDrift(t *testing.T) {
	dir := freshConfigDir(t)
	rendered, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SettingsFile(), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &obj); err != nil {
		t.Fatal(err)
	}
	themeRaw, ok := obj["theme"]
	if !ok {
		t.Skip("template has no theme key to duplicate")
	}
	local, err := json.Marshal(map[string]json.RawMessage{"theme": themeRaw})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.local.json"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.LocalDup) == 0 {
		t.Error("LocalDup is empty, want the duplicated theme key reported")
	}
	if d.Drifted() {
		t.Errorf("a local-file duplicate alone must not count as drift: %v", d.Lines())
	}
}

// TestCheckSettingsDriftLinesTruncatesLongForeignKey: a foreign key far longer than 80 runes must
// still render with a <=80-rune embedded key and a whole line <=80 runes, verb intact.
func TestCheckSettingsDriftLinesTruncatesLongForeignKey(t *testing.T) {
	freshConfigDir(t)
	rendered, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	longKey := strings.Repeat("k", 200)
	trimmed := strings.TrimRight(strings.TrimSpace(string(rendered)), "}")
	withForeign := trimmed + `,"` + longKey + `":true}`
	if err := os.WriteFile(SettingsFile(), []byte(withForeign), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range d.Foreign {
		if k == longKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("Foreign = %v, want it to contain the untruncated 200-char key", d.Foreign)
	}
	var line string
	for _, l := range d.Lines() {
		if strings.Contains(l, "is a foreign key not in the template") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("Lines() = %v, want a foreign-key finding", d.Lines())
	}
	if !strings.HasPrefix(line, "settings: ") {
		t.Errorf("line %q must carry the settings: prefix", line)
	}
	embedded := strings.TrimSuffix(strings.TrimPrefix(line, "settings: "), " is a foreign key not in the template")
	if n := len([]rune(embedded)); n > 80 {
		t.Errorf("embedded key is %d runes, want <=80", n)
	}
	if n := len([]rune(line)); n > 80 {
		t.Errorf("whole line is %d runes, want <=80", n)
	}
	if !strings.HasSuffix(embedded, "…") {
		t.Errorf("embedded key = %q, want it truncated with an ellipsis", embedded)
	}
}

// TestCheckSettingsDriftLinesFitEightyColumns: every reported line, across every finding kind, stays
// within 80 columns and carries the "settings: " prefix.
func TestCheckSettingsDriftLinesFitEightyColumns(t *testing.T) {
	freshConfigDir(t)
	file := SettingsFile()
	backup := paths.BackupPath(file, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{not valid json,"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Lines()) == 0 {
		t.Fatal("Lines() is empty for a drifted file")
	}
	for _, l := range d.Lines() {
		if n := len([]rune(l)); n > 80 {
			t.Errorf("line %q is %d columns, want <=80", l, n)
		}
		if !strings.HasPrefix(l, "settings: ") {
			t.Errorf("line %q must be prefixed %q", l, "settings: ")
		}
	}
}

// TestCheckSettingsDriftDroppedKey: a live key on the drop list is Differs (not Foreign) until install removes it.
func TestCheckSettingsDriftDroppedKey(t *testing.T) {
	freshConfigDir(t)
	rendered, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.UpdateDropped(func([]string) []string { return []string{"verbose"} }); err != nil {
		t.Fatal(err)
	}
	withKey := strings.TrimRight(strings.TrimSpace(string(rendered)), "}") + `,"verbose":true}`
	if err := os.WriteFile(SettingsFile(), []byte(withKey), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	want := "verbose is dropped (run claude-code install)"
	if !d.Drifted() || !slices.Contains(d.Differs, want) {
		t.Errorf("Differs = %v, want %q", d.Differs, want)
	}
	if slices.Contains(d.Foreign, "verbose") {
		t.Errorf("dropped key must not also be Foreign: %v", d.Foreign)
	}
	merged, _, err := settings.Merge([]byte(withKey), rendered, []string{"verbose"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SettingsFile(), merged, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err = CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if d.Drifted() || len(d.Lines()) != 0 {
		t.Errorf("after the merge-rendered file nothing should print: %v", d.Lines())
	}
}

// breakVault corrupts the vault file so settings.LoadDropped fails.
func breakVault(t *testing.T, dir string) {
	t.Helper()
	if err := settings.UpdateDropped(func([]string) []string { return []string{"verbose"} }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "vault.db"), []byte("not a bolt db, garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A drop-list load error is an informational Note: drift detection continues and Drifted() ignores it.
func TestCheckSettingsDriftDropListErrorIsNote(t *testing.T) {
	dir := freshConfigDir(t)
	rendered, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SettingsFile(), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	breakVault(t, dir)
	d, err := CheckSettingsDriftWith(rendered)
	if err != nil {
		t.Fatalf("CheckSettingsDriftWith = %v, want it to continue", err)
	}
	if d.Drifted() {
		t.Errorf("a drop-list error must not count as drift: %+v", d)
	}
	lines := d.Lines()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "settings: drop list unavailable:") {
		t.Errorf("Lines = %q, want one informational drop-list line", lines)
	}
}

// Every assembled line, whatever the finding kind, stays within 80 runes.
func TestSettingsDriftLinesCapAssembledLongFindings(t *testing.T) {
	long := strings.Repeat("x", 200)
	d := SettingsDrift{
		File:     "/f/settings.json",
		Invalid:  errors.New("parse " + long),
		Foreign:  []string{long},
		Kept:     []string{long},
		LocalDup: []string{"settings.local.json " + long + " repeats the template"},
		Nested:   long,
		Note:     "drop list unavailable: " + long,
	}
	lines := d.Lines()
	if len(lines) != 6 {
		t.Fatalf("lines = %d, want 6: %q", len(lines), lines)
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > 80 {
			t.Errorf("line is %d runes, want <=80: %q", n, l)
		}
		if !strings.HasPrefix(l, "settings: ") {
			t.Errorf("line %q lost its prefix", l)
		}
	}
}

// A long key is shortened, never the verb that says what is wrong with it.
func TestSettingsDriftLinesKeepVerbForLongKey(t *testing.T) {
	long := strings.Repeat("k", 120)
	d := SettingsDrift{Differs: []string{long}, Foreign: []string{long}, Kept: []string{long}}
	want := []string{" differs from the template", " is a foreign key not in the template", " is kept (not in the template)"}
	lines := d.Lines()
	if len(lines) != len(want) {
		t.Fatalf("lines = %q", lines)
	}
	for i, l := range lines {
		if !strings.HasSuffix(l, want[i]) {
			t.Errorf("line %q lost its verb %q", l, want[i])
		}
		if n := len([]rune(l)); n > 80 {
			t.Errorf("line is %d runes, want <=80", n)
		}
		if !strings.Contains(l, "…") {
			t.Errorf("line %q: key not marked truncated", l)
		}
	}
}

// The joined install detail is capped per line, not only per finding kind.
func TestSettingsDriftDetailCapsEachLine(t *testing.T) {
	long := strings.Repeat("k", 200)
	detail := settingsDriftDetailLabeled(SettingsDrift{Foreign: []string{long}, Differs: []string{long}}, nil, "settings")
	for part := range strings.SplitSeq(detail, "; ") {
		if n := len([]rune(part)); n > 80 {
			t.Errorf("detail part is %d runes, want <=80: %q", n, part)
		}
	}
}
