// claude_settings.go — the "claude code" page: every catalog key, grouped by Sections(), edited and held
// until the page is left, then saved to the vault overlay and re-rendered into settings.json.
package configure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/tui"
)

// pickSettingsPage is the sectioned picker seam; tests replace it.
var pickSettingsPage = tui.PickSections

// claudeSettingsRow is the main-menu "claude code" row: N overrides, or "vault error" when the overlay
// fails to load.
func claudeSettingsRow(d *dialog) homeRow {
	ov, err := settings.LoadOverlay()
	detail := "vault error"
	if err == nil {
		detail = fmt.Sprintf("%d overrides", len(ov))
	}
	return homeRow{"claude code", detail, "every claude launch", func(d *dialog) { runClaudeSettings(d) }}
}

// settingRow renders one catalog field as a page row: key, then its effective value and source
// (template|vault), flagging when the live settings.json holds something else. tmpl/ov/live are
// parsed top-level settings objects; a missing live map (no file yet) never raises the flag.
func settingRow(f settings.Field, tmpl, ov, live map[string]json.RawMessage) [2]string {
	eff, source := tmpl[f.Key], "template"
	if v, ok := ov[f.Key]; ok {
		eff, source = v, "vault"
	}
	if l, ok := live[f.Key]; ok && !jsonEqual(l, eff) {
		source += " · live differs"
	}
	return [2]string{f.Key, settingText(eff) + "  " + source}
}

// settingText renders a raw JSON scalar as plain text: quotes stripped off a string, everything else as-is.
func settingText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// jsonEqual compares two raw JSON scalars after compacting, so formatting differences are not drift.
func jsonEqual(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return ca.String() == cb.String()
}

// editSetting prompts for a new value for f (tmplVal is its template value, shown as the reset target).
// A nil json.RawMessage means reset to the template (remove the overlay key); ok is false on cancel or
// a parse error (already reported), meaning no change at all.
func editSetting(d *dialog, f settings.Field, tmplVal json.RawMessage) (json.RawMessage, bool) {
	if f.Kind == settings.KindBool || f.Kind == settings.KindEnum {
		return editChoice(d, f, tmplVal)
	}
	return editText(d, f, tmplVal)
}

// editChoice is the on/off or choice picker, with a trailing "reset to template" row; theme also gets a
// "custom..." row reaching the documented custom:<name> value (catalog Validate accepts it, but no enum
// row names a specific one).
func editChoice(d *dialog, f settings.Field, tmplVal json.RawMessage) (json.RawMessage, bool) {
	values := f.Values
	if f.Kind == settings.KindBool {
		values = []string{"true", "false"}
	}
	rows := make([][2]string, 0, len(values)+2)
	for _, v := range values {
		rows = append(rows, [2]string{v, ""})
	}
	customIdx := -1
	if f.Key == "theme" {
		customIdx = len(rows)
		rows = append(rows, [2]string{"custom...", "enter a custom:<name> theme"})
	}
	resetIdx := len(rows)
	rows = append(rows, [2]string{"reset to template (" + settingText(tmplVal) + ")", ""})
	idx, err := pickPlain(d, f.Key, rows)
	if err != nil || idx < 0 {
		return nil, false
	}
	if idx == resetIdx {
		return nil, true
	}
	if idx == customIdx {
		name := strings.TrimSpace(textLine(d, "theme custom name: "))
		if name == "" {
			return nil, false
		}
		raw, err := f.Parse("custom:" + name)
		if err != nil {
			d.printf("%s: %v\n", f.Key, err)
			return nil, false
		}
		return raw, true
	}
	raw, err := f.Parse(values[idx])
	if err != nil {
		d.printf("%s: %v\n", f.Key, err)
		return nil, false
	}
	return raw, true
}

// editText is the number/text prompt; an empty line resets to the template.
func editText(d *dialog, f settings.Field, tmplVal json.RawMessage) (json.RawMessage, bool) {
	input := strings.TrimSpace(textLine(d, f.Key+" (empty resets): "))
	if input == "" {
		return nil, true
	}
	raw, err := f.Parse(input)
	if err != nil {
		d.printf("%s: %v\n", f.Key, err)
		return nil, false
	}
	return raw, true
}

// renderObject parses render's output into a top-level key -> value map.
func renderObject(render func() ([]byte, error)) (map[string]json.RawMessage, error) {
	raw, err := render()
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// readLiveSettings parses the live settings.json; a missing file is not an error (just no rows to flag).
func readLiveSettings() (map[string]json.RawMessage, error) {
	return renderObject(func() ([]byte, error) { return os.ReadFile(launch.SettingsFile()) })
}

// runClaudeSettings is the "claude code" page: interactive on a terminal (edits staged until the page is
// left, then one confirm writes the overlay); otherwise a read-only report, same gate every other section
// uses, so --check/--dry-run (and any piped run) never prompt or write. Returns false on a load/render
// error, or in --check when any key's live value differs from the effective one.
func runClaudeSettings(d *dialog) bool {
	ov, err := settings.LoadOverlay()
	if err != nil {
		d.printf("claude code: %v\n", err)
		return false
	}
	sections, err := settings.Sections()
	if err != nil {
		d.printf("claude code: %v\n", err)
		return false
	}
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return runClaudeSettingsMenu(d, ov, sections)
	}
	return reportClaudeSettings(d, ov, sections)
}

// reportClaudeSettings prints one line per Catalog key under its section header: key, effective value,
// source (template|vault[, live differs]) — no prompts, no writes.
func reportClaudeSettings(d *dialog, ov map[string]json.RawMessage, sections []settings.Section) bool {
	tmpl, err := renderObject(settings.Render)
	if err != nil {
		d.printf("claude code: %v\n", err)
		return false
	}
	live, err := readLiveSettings()
	if err != nil && !os.IsNotExist(err) {
		d.printf("claude code: %v\n", err)
	}
	ok := true
	for _, sec := range sections {
		header := false
		for _, key := range sec.Keys {
			f, found := settings.Lookup(key)
			if !found {
				continue
			}
			if !header {
				d.println("== " + sec.Name + " ==")
				header = true
			}
			row := settingRow(f, tmpl, ov, live)
			d.println(row[0] + "  " + row[1])
			if strings.Contains(row[1], "live differs") {
				ok = false
			}
		}
	}
	if dropped, err := settings.LoadDropped(); err == nil && len(dropped) > 0 {
		d.println("dropped keys: " + strings.Join(dropped, ", "))
	}
	if d.o.Check {
		return ok
	}
	return true
}

// runDropKeep applies --drop/--keep to the dropped-key list (next = (list + drop) - keep); the caller re-renders.
func runDropKeep(d *dialog) int {
	cur, err := settings.LoadDropped()
	if err != nil {
		d.printf("claude code: %v\n", err)
		return 1
	}
	if d.o.Check { // exit 1 while a --drop key is not yet listed or a --keep key still is
		d.println("dropped keys: " + strings.Join(cur, ", "))
		for _, k := range d.o.Drop {
			if !slices.Contains(cur, k) {
				return 1
			}
		}
		for _, k := range d.o.Keep {
			if slices.Contains(cur, k) {
				return 1
			}
		}
		return 0
	}
	live, err := readLiveSettings()
	if err != nil && !os.IsNotExist(err) {
		d.printf("claude code: %v\n", err)
		return 1
	}
	var absent []string // a key Claude Code has not written (yet) is still worth dropping: it goes on arrival
	for _, k := range d.o.Drop {
		if _, ok := live[k]; !ok && !slices.Contains(cur, k) {
			absent = append(absent, k)
		}
	}
	if len(absent) > 0 {
		d.printf("claude code: not in settings.json now, removed if it appears: %s\n", strings.Join(absent, ", "))
	}
	var notDropped []string
	for _, k := range d.o.Keep {
		if !slices.Contains(cur, k) {
			notDropped = append(notDropped, k)
		}
	}
	if len(notDropped) > 0 {
		d.printf("claude code: not dropped: %s\n", strings.Join(notDropped, ", "))
		return 1
	}
	apply := func(cur []string) []string {
		return slices.DeleteFunc(slices.Concat(cur, d.o.Drop), func(k string) bool { return slices.Contains(d.o.Keep, k) })
	}
	next := apply(cur)
	if d.o.DryRun {
		err = settings.CheckDroppable(next)
	} else if err = settings.UpdateDropped(apply); err == nil { // read-apply-write in one vault transaction
		d.changed = true
	}
	if err != nil {
		d.printf("claude code: %v\n", err)
		return 1
	}
	if len(d.o.Drop) > 0 {
		d.printf("claude code: dropped: %s\n", strings.Join(d.o.Drop, ", "))
	}
	if len(d.o.Keep) > 0 {
		d.printf("claude code: kept: %s\n", strings.Join(d.o.Keep, ", "))
	}
	if d.o.DryRun {
		d.println("dropped keys: " + strings.Join(slices.Compact(slices.Sorted(slices.Values(next))), ", "))
	}
	return 0
}

// runClaudeSettingsMenu is the interactive page: rows grouped under Sections() headers, edits staged until
// the page is left (Esc or the trailing "save and go back" row), then one confirm writes the overlay.
func runClaudeSettingsMenu(d *dialog, ov map[string]json.RawMessage, sections []settings.Section) bool {
	staged := map[string]json.RawMessage{} // explicit new overlay value for this key, this page
	reset := map[string]bool{}             // key removed from the overlay (back to template), this page
	defer saveClaudeSettings(d, staged, reset)
	start := 0
	for {
		tmpl, err := renderObject(settings.Render)
		if err != nil {
			d.printf("claude code: %v\n", err)
			return false
		}
		live, err := readLiveSettings()
		if err != nil && !os.IsNotExist(err) {
			d.printf("claude code: %v\n", err)
		}
		cur := maps.Clone(ov)
		for k := range reset {
			delete(cur, k)
		}
		maps.Copy(cur, staged)

		var tuiSections []tui.Section
		var fields []settings.Field
		for _, sec := range sections {
			var rows [][2]string
			for _, key := range sec.Keys {
				f, ok := settings.Lookup(key)
				if !ok {
					continue
				}
				rows = append(rows, settingRow(f, tmpl, cur, live))
				fields = append(fields, f)
			}
			if len(rows) > 0 {
				tuiSections = append(tuiSections, tui.Section{Title: "== " + sec.Name + " ==", Rows: rows})
			}
		}
		idx, err := pickSettingsPage("claude code — per-machine overrides", tuiSections, enterRow("save and go back"), start)
		if err != nil || idx < 0 || idx >= len(fields) {
			return true
		}
		start = idx
		f := fields[idx]
		raw, ok := editSetting(d, f, tmpl[f.Key])
		if !ok {
			continue
		}
		stageSetting(ov, staged, reset, f.Key, raw, tmpl[f.Key])
	}
}

// stageSetting records one edit: nil (explicit reset) or a value matching the template both remove the
// overlay key; otherwise it stages the new value.
func stageSetting(ov, staged map[string]json.RawMessage, reset map[string]bool, key string, raw, tmplVal json.RawMessage) {
	delete(staged, key)
	if raw == nil || jsonEqual(raw, tmplVal) {
		if _, had := ov[key]; had {
			reset[key] = true
		} else {
			delete(reset, key)
		}
		return
	}
	delete(reset, key)
	staged[key] = raw
}

// saveClaudeSettings asks to save n staged change(s) on leaving the page; n/Esc discards them. The
// overlay is reloaded right before the write, so only this session's staged sets/resets are applied —
// never a stale snapshot taken when the page opened, which would clobber a concurrent overlay write.
func saveClaudeSettings(d *dialog, staged map[string]json.RawMessage, reset map[string]bool) {
	n := len(staged) + len(reset)
	if n == 0 {
		return
	}
	if !confirmLeave(n, "claude code setting(s)") {
		d.printf("claude code: %d change(s) discarded\n", n)
		return
	}
	next, err := settings.LoadOverlay()
	if err != nil {
		d.printf("claude code: %v\n", err)
		return
	}
	next = maps.Clone(next)
	for k := range reset {
		delete(next, k)
	}
	maps.Copy(next, staged)
	if err := settings.SaveOverlay(next); err != nil {
		d.printf("claude code: %v\n", err)
		return
	}
	d.changed = true
	d.printf("claude code: %d setting(s) saved\n", n)
}
