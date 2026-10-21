package configure

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// newClaudeSettingsDialog isolates the vault (fresh CLAUDE_CONFIG_DIR) and feeds in as the piped input a
// non-tty dialogue reads from (pick/textLine fall back to the numbered/dialogue path since d.o.In != os.Stdin).
func newClaudeSettingsDialog(t *testing.T, in string) (*dialog, *bytes.Buffer) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	r := strings.NewReader(in)
	var out bytes.Buffer
	return &dialog{o: Options{In: r}, br: bufio.NewReader(r), out: &out}, &out
}

// stubSettingsPage replaces pickSettingsPage with picks, returned in order, then -1 (Esc) forever after.
func stubSettingsPage(t *testing.T, picks ...int) {
	t.Helper()
	orig := pickSettingsPage
	t.Cleanup(func() { pickSettingsPage = orig })
	i := 0
	pickSettingsPage = func(title string, secs []tui.Section, trailing [2]string, start int) (int, error) {
		if i < len(picks) {
			p := picks[i]
			i++
			return p, nil
		}
		return -1, nil
	}
}

// runMenuDirect calls runClaudeSettingsMenu straight (bypassing runClaudeSettings' interactive+tty gate,
// which a non-tty test run can never pass) so the held-edit/save behavior stays exercised.
func runMenuDirect(t *testing.T, d *dialog) bool {
	t.Helper()
	ov, err := settings.LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	sections, err := settings.Sections()
	if err != nil {
		t.Fatal(err)
	}
	return runClaudeSettingsMenu(d, ov, sections)
}

// fieldIndex finds f's position in the flattened, across-sections field order runClaudeSettings builds,
// by replaying the same walk over settings.Sections().
func fieldIndex(t *testing.T, key string) int {
	t.Helper()
	sections, err := settings.Sections()
	if err != nil {
		t.Fatal(err)
	}
	idx := 0
	for _, sec := range sections {
		for _, k := range sec.Keys {
			if _, ok := settings.Lookup(k); !ok {
				continue
			}
			if k == key {
				return idx
			}
			idx++
		}
	}
	t.Fatalf("catalog key %q not found among section keys", key)
	return -1
}

// TestClaudeCodeRowInSettingsRows: the main-menu settings block must carry a "claude code" row.
func TestClaudeCodeRowInSettingsRows(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rows := settingsRows(&dialog{out: discardWriter{}})
	found := false
	for _, r := range rows {
		if r.name == "claude code" {
			found = true
			if r.run == nil {
				t.Error("claude code row has no run action")
			}
		}
	}
	if !found {
		t.Fatalf("settingsRows() = %+v, want a \"claude code\" row", rows)
	}
}

// TestRunClaudeSettingsHeldEditSavedOnYes: an edit stays held until the page is left; saying yes writes it
// to the overlay.
func TestRunClaudeSettingsHeldEditSavedOnYes(t *testing.T) {
	// alwaysThinkingEnabled's template default is true, so row 2 ("false") is an actual override —
	// picking "true" (row 1) would equal the template and reset instead of stage a new value.
	d, _ := newClaudeSettingsDialog(t, "2\n")
	idx := fieldIndex(t, "alwaysThinkingEnabled")
	stubSettingsPage(t, idx)
	origConfirmed := saveConfirmed
	t.Cleanup(func() { saveConfirmed = origConfirmed })
	saveConfirmed = func(string) bool { return true }

	runMenuDirect(t, d)

	ov, err := settings.LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if string(ov["alwaysThinkingEnabled"]) != "false" {
		t.Fatalf("overlay[alwaysThinkingEnabled] = %s, want false", ov["alwaysThinkingEnabled"])
	}
	if !d.changed {
		t.Error("d.changed must be set after a saved edit")
	}
}

// TestRunClaudeSettingsHeldEditDroppedOnNo: the same held edit, declined at the leave prompt, must never
// reach the overlay.
func TestRunClaudeSettingsHeldEditDroppedOnNo(t *testing.T) {
	d, _ := newClaudeSettingsDialog(t, "2\n") // "false": an actual override of the true template default
	idx := fieldIndex(t, "alwaysThinkingEnabled")
	stubSettingsPage(t, idx)
	origConfirmed := saveConfirmed
	t.Cleanup(func() { saveConfirmed = origConfirmed })
	saveConfirmed = func(string) bool { return false }

	runMenuDirect(t, d)

	ov, err := settings.LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ov["alwaysThinkingEnabled"]; ok {
		t.Fatalf("overlay = %v, want alwaysThinkingEnabled absent after declining the save", ov)
	}
	if d.changed {
		t.Error("d.changed must stay false when the edit was discarded")
	}
}

// TestChoosingTemplateValueDeletesOverlayKey: a field already overridden, reset via the "reset to
// template" picker row and saved, must come back out of the overlay entirely.
func TestChoosingTemplateValueDeletesOverlayKey(t *testing.T) {
	d, _ := newClaudeSettingsDialog(t, "")
	if err := settings.SaveOverlay(map[string]json.RawMessage{"alwaysThinkingEnabled": json.RawMessage("true")}); err != nil {
		t.Fatal(err)
	}
	idx := fieldIndex(t, "alwaysThinkingEnabled")
	stubSettingsPage(t, idx)
	origConfirmed := saveConfirmed
	t.Cleanup(func() { saveConfirmed = origConfirmed })
	saveConfirmed = func(string) bool { return true }

	// editChoice's picker rows for a bool field are ["true","false","reset to template (...)"];
	// feed the numbered prompt the 3rd row (reset).
	d.br = bufio.NewReader(strings.NewReader("3\n"))

	runMenuDirect(t, d)

	ov, err := settings.LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ov["alwaysThinkingEnabled"]; ok {
		t.Fatalf("overlay = %v, want alwaysThinkingEnabled removed after choosing the template value", ov)
	}
}

// TestPickingValueEqualToTemplateAlsoDeletesOverlayKey: picking the plain choice row that happens to equal
// the template value (not the explicit "reset to template" row) must have the same effect — no new override.
func TestPickingValueEqualToTemplateAlsoDeletesOverlayKey(t *testing.T) {
	d, _ := newClaudeSettingsDialog(t, "")
	if err := settings.SaveOverlay(map[string]json.RawMessage{"alwaysThinkingEnabled": json.RawMessage("false")}); err != nil {
		t.Fatal(err)
	}
	idx := fieldIndex(t, "alwaysThinkingEnabled")
	stubSettingsPage(t, idx)
	origConfirmed := saveConfirmed
	t.Cleanup(func() { saveConfirmed = origConfirmed })
	saveConfirmed = func(string) bool { return true }

	// Row 1 is "true", the template's own default — not the trailing reset row.
	d.br = bufio.NewReader(strings.NewReader("1\n"))

	runMenuDirect(t, d)

	ov, err := settings.LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ov["alwaysThinkingEnabled"]; ok {
		t.Fatalf("overlay = %v, want alwaysThinkingEnabled removed after picking the template's own value", ov)
	}
}

// TestEditSettingInvalidIntRefused: a non-numeric answer for an int field must be refused — Parse errors,
// editText reports it, and the field is left untouched (ok=false, no overlay write).
func TestEditSettingInvalidIntRefused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var out bytes.Buffer
	in := strings.NewReader("not-a-number\n")
	d := &dialog{o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	f, ok := settings.Lookup("cleanupPeriodDays")
	if !ok {
		t.Fatal("catalog has no cleanupPeriodDays field")
	}
	raw, ok := editSetting(d, f, json.RawMessage("20"))
	if ok {
		t.Fatalf("editSetting(invalid int) = (%s, true), want ok=false", raw)
	}
	if raw != nil {
		t.Errorf("raw = %s, want nil on a refused edit", raw)
	}
	if !strings.Contains(out.String(), "cleanupPeriodDays") {
		t.Errorf("output = %q, want the error to name the field", out.String())
	}
}

// TestSettingRowFlagsLiveDiffers: settingRow must append "· live differs" only when the live settings.json
// holds a value unequal to the effective (overlay-or-template) one.
func TestSettingRowFlagsLiveDiffers(t *testing.T) {
	f, ok := settings.Lookup("theme")
	if !ok {
		t.Fatal("catalog has no theme field")
	}
	tmpl := map[string]json.RawMessage{"theme": json.RawMessage(`"dark"`)}
	ov := map[string]json.RawMessage{}
	live := map[string]json.RawMessage{"theme": json.RawMessage(`"light"`)}
	row := settingRow(f, tmpl, ov, live)
	if !strings.Contains(row[1], "live differs") {
		t.Errorf("settingRow = %v, want it to flag a live mismatch", row)
	}

	live["theme"] = json.RawMessage(`"dark"`)
	row = settingRow(f, tmpl, ov, live)
	if strings.Contains(row[1], "live differs") {
		t.Errorf("settingRow = %v, want no live-differs flag once live matches", row)
	}
}

// writeLiveSettings seeds launch.SettingsFile() with raw, creating its parent dir.
func writeLiveSettings(t *testing.T, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(launch.SettingsFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launch.SettingsFile(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCheckAndDryRunNeverWriteOrPrompt: `configure claude` with Check or DryRun must leave settings.json
// and the vault overlay byte-identical, and must never reach the interactive picker seam.
func TestCheckAndDryRunNeverWriteOrPrompt(t *testing.T) {
	for _, mode := range []string{"check", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			rendered, err := settings.Render()
			if err != nil {
				t.Fatal(err)
			}
			writeLiveSettings(t, rendered)
			before, err := os.ReadFile(launch.SettingsFile())
			if err != nil {
				t.Fatal(err)
			}
			ovBefore, err := settings.LoadOverlay()
			if err != nil {
				t.Fatal(err)
			}

			calls := 0
			orig := pickSettingsPage
			t.Cleanup(func() { pickSettingsPage = orig })
			pickSettingsPage = func(title string, secs []tui.Section, trailing [2]string, start int) (int, error) {
				calls++
				return -1, nil
			}

			o := Options{In: strings.NewReader(""), Out: &bytes.Buffer{}}
			if mode == "check" {
				o.Check = true
			} else {
				o.DryRun = true
			}
			Run(o, "claude")

			if calls != 0 {
				t.Errorf("picker seam called %d time(s), want 0 under --%s", calls, mode)
			}
			after, err := os.ReadFile(launch.SettingsFile())
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("settings.json changed under --%s", mode)
			}
			ovAfter, err := settings.LoadOverlay()
			if err != nil {
				t.Fatal(err)
			}
			if len(ovAfter) != len(ovBefore) {
				t.Errorf("overlay changed under --%s: before=%v after=%v", mode, ovBefore, ovAfter)
			}
		})
	}
}

// TestCheckExitsNonzeroOnLiveDiffers: Run(..., "claude") with Check must exit nonzero when the live
// settings.json disagrees with the effective value of some key, and zero once it matches again.
func TestCheckExitsNonzeroOnLiveDiffers(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tmpl, err := settings.Render()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(tmpl, &m); err != nil {
		t.Fatal(err)
	}
	// alwaysThinkingEnabled's template default is true; flip it live to force a mismatch.
	m["alwaysThinkingEnabled"] = json.RawMessage("false")
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeLiveSettings(t, raw)

	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "claude")
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 with a live-differs key; output:\n%s", rc, out.String())
	}

	m["alwaysThinkingEnabled"] = json.RawMessage("true")
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeLiveSettings(t, raw)
	rc = Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "claude")
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 once live matches; output:\n%s", rc, out.String())
	}
}

// TestSaveReloadsOverlayNoLostUpdate: a concurrent writer's SaveOverlay between this session's page-open
// and its own save must not be clobbered — both keys must land in the final overlay.
func TestSaveReloadsOverlayNoLostUpdate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ov, err := settings.LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	sections, err := settings.Sections()
	if err != nil {
		t.Fatal(err)
	}

	themeIdx := fieldIndex(t, "theme")
	stubSettingsPage(t, themeIdx)
	d := &dialog{o: Options{In: strings.NewReader("2\n"), Out: &bytes.Buffer{}}}
	d.br = bufio.NewReader(strings.NewReader("2\n")) // theme row 2 = "dark"
	d.out = &bytes.Buffer{}

	origConfirmed := saveConfirmed
	t.Cleanup(func() { saveConfirmed = origConfirmed })
	saveConfirmed = func(string) bool {
		// Another writer lands a change to a different key while this session is still deciding.
		if err := settings.SaveOverlay(map[string]json.RawMessage{"tui": json.RawMessage(`"default"`)}); err != nil {
			t.Fatal(err)
		}
		return true
	}

	runClaudeSettingsMenu(d, ov, sections)

	final, err := settings.LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if string(final["theme"]) != `"dark"` {
		t.Errorf("overlay[theme] = %s, want \"dark\"", final["theme"])
	}
	if string(final["tui"]) != `"default"` {
		t.Errorf("overlay = %v, want the concurrent writer's tui key preserved too", final)
	}
}

// TestRunClaudeSettingsVaultErrorIsNonzero: a vault load error must surface as a nonzero Run exit, not
// get swallowed by a hardcoded ok=true for the "claude" section.
func TestRunClaudeSettingsVaultErrorIsNonzero(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := vault.Update(func(tx *vault.Tx) error {
		return tx.Put(vault.Overlay, "settings", "{not valid json")
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "claude")
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 on a vault overlay load error; output:\n%s", rc, out.String())
	}
}

// TestThemeCustomFlowStoresCustomPrefixedValue: picking theme's "custom..." row and entering a name must
// stage and save "custom:<name>", the one value Validate accepts outside the fixed enum.
func TestThemeCustomFlowStoresCustomPrefixedValue(t *testing.T) {
	d, _ := newClaudeSettingsDialog(t, "")
	themeIdx := fieldIndex(t, "theme")
	stubSettingsPage(t, themeIdx)
	origConfirmed := saveConfirmed
	t.Cleanup(func() { saveConfirmed = origConfirmed })
	saveConfirmed = func(string) bool { return true }

	// theme's rows are its 7 enum values, then "custom...", then "reset to template": row 8 is "custom...".
	d.br = bufio.NewReader(strings.NewReader("8\nmytheme\n"))

	runMenuDirect(t, d)

	ov, err := settings.LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if string(ov["theme"]) != `"custom:mytheme"` {
		t.Fatalf("overlay[theme] = %s, want \"custom:mytheme\"", ov["theme"])
	}
}
