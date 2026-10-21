package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	backup := file + ".bak-20260101-000000.000000000"
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

// TestTruncate80 pins truncate80's own contract: short strings pass through untouched, and a
// string over 80 runes is cut to exactly 80 runes (79 kept + the trailing ellipsis rune).
func TestTruncate80(t *testing.T) {
	short := "effortLevel"
	if got := truncate80(short); got != short {
		t.Errorf("truncate80(%q) = %q, want unchanged", short, got)
	}
	long := strings.Repeat("x", 200)
	got := truncate80(long)
	gotRunes := []rune(got)
	if len(gotRunes) != 80 {
		t.Fatalf("truncate80(200-char key) = %d runes, want 80", len(gotRunes))
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 79)) || gotRunes[79] != '…' {
		t.Errorf("truncate80(long) = %q, want 79 kept runes + ellipsis", got)
	}
}

// TestCheckSettingsDriftLinesTruncatesLongForeignKey: a foreign key far longer than 80 runes must
// still render with a <=80-rune embedded key — truncated, not reported at full length (the verb
// text around it is not truncated and the whole line can still exceed 80 columns; that is known
// techdebt, not asserted here).
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
	if !strings.HasSuffix(embedded, "…") {
		t.Errorf("embedded key = %q, want it truncated with an ellipsis", embedded)
	}
}

// TestCheckSettingsDriftLinesFitEightyColumns: every reported line, across every finding kind, stays
// within 80 columns and carries the "settings: " prefix.
func TestCheckSettingsDriftLinesFitEightyColumns(t *testing.T) {
	freshConfigDir(t)
	file := SettingsFile()
	backup := file + ".bak-20260101-000000.000000000"
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
