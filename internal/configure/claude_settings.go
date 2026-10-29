// claude_settings.go — the "claude code" page: your defaults, grouped by Sections(), edited and held until
// the page is left, then saved as default to the vault overlay; it can also add any schema key or write repo defaults.
package configure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/tui"
)

// pickSettingsPage is the sectioned picker seam; tests replace it.
var pickSettingsPage = tui.PickSections

// addSettingList is the filterable "+ add a setting" picker seam; tests replace it.
var addSettingList = tui.List

// gitDiffStat prints the uncommitted change to the repo defaults file; tests replace it.
var gitDiffStat = func(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "diff", "--stat", "--", settings.RepoDefaultsRel).CombinedOutput()
	return string(out), err
}

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
// (default|mine), flagging when the live settings.json holds something else. tmpl/ov/live are
// parsed top-level settings objects; a missing live map (no file yet) never raises the flag.
func settingRow(f settings.Field, tmpl, ov, live map[string]json.RawMessage) [2]string {
	eff, source := tmpl[f.Key], "default"
	if v, ok := ov[f.Key]; ok {
		eff, source = v, "mine"
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

// editSetting prompts for a new value for f (tmplVal is its repo default, nil when it has none).
// A nil json.RawMessage means remove it from my defaults (drop the overlay key); ok is false on cancel or
// a parse error (already reported), meaning no change at all.
func editSetting(d *dialog, f settings.Field, tmplVal json.RawMessage) (json.RawMessage, bool) {
	if f.Kind == settings.KindBool || f.Kind == settings.KindEnum {
		return editChoice(d, f, tmplVal)
	}
	return editText(d, f, tmplVal)
}

// editChoice is the on/off or choice picker, with a trailing "remove from mine" row; theme also gets a
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
	rows = append(rows, [2]string{removeLabel(tmplVal), ""})
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

// removeLabel is the trailing editChoice row that removes the key from my defaults.
func removeLabel(tmplVal json.RawMessage) string {
	if tmplVal == nil {
		return "remove from mine"
	}
	return "remove from mine (default " + textutil.Truncate(settingText(tmplVal), 30) + ")"
}

// editText is the number/text prompt; an empty line removes the key from my defaults.
func editText(d *dialog, f settings.Field, tmplVal json.RawMessage) (json.RawMessage, bool) {
	input := strings.TrimSpace(textLine(d, f.Key+" (empty removes): "))
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
	tmpl, live = flattenEnv(tmpl), flattenEnv(live)
	ok := true
	shown := map[string]bool{}
	emit := func(f settings.Field) {
		row := settingRow(f, tmpl, ov, live)
		d.println(row[0] + "  " + row[1])
		if strings.Contains(row[1], "live differs") {
			ok = false
		}
	}
	for _, sec := range sections {
		header := false
		for _, key := range sec.Keys {
			shown[key] = true
			f, found := settings.FieldFor(key)
			if !found {
				continue
			}
			if !header {
				d.println("== " + sec.Name + " ==")
				header = true
			}
			emit(f)
		}
	}
	header := false
	for _, key := range slices.Sorted(maps.Keys(ov)) {
		if f, found := settings.FieldFor(key); found && !shown[key] {
			if !header {
				d.println("== added ==")
				header = true
			}
			emit(f)
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

// dropKeepCheck is the --check branch: exit 1 while a --drop key is not yet listed or a --keep key still is.
func dropKeepCheck(d *dialog, cur []string) int {
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

// validateDropKeep notes --drop keys absent from settings.json (still worth dropping) and errors on unlisted --keep keys.
func validateDropKeep(d *dialog, cur []string, live map[string]json.RawMessage) error {
	var absent []string
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
		return fmt.Errorf("not dropped: %s", strings.Join(notDropped, ", "))
	}
	return nil
}

// applyDropKeep writes (or, on --dry-run, only validates) next = (list + drop) - keep and returns it.
func applyDropKeep(d *dialog, cur []string) ([]string, error) {
	apply := func(cur []string) []string {
		return slices.DeleteFunc(slices.Concat(cur, d.o.Drop), func(k string) bool { return slices.Contains(d.o.Keep, k) })
	}
	next := apply(cur)
	if d.o.DryRun {
		return next, settings.CheckDroppable(next)
	}
	err := settings.UpdateDropped(apply) // read-apply-write in one vault transaction
	d.changed = err == nil
	return next, err
}

// runDropKeep applies --drop/--keep to the dropped-key list (next = (list + drop) - keep); the caller re-renders.
func runDropKeep(d *dialog) int {
	cur, err := settings.LoadDropped()
	if err != nil {
		d.printf("claude code: %v\n", err)
		return 1
	}
	if d.o.Check {
		return dropKeepCheck(d, cur)
	}
	live, err := readLiveSettings()
	if err != nil && !os.IsNotExist(err) {
		d.printf("claude code: %v\n", err)
		return 1
	}
	if err := validateDropKeep(d, cur, live); err != nil {
		d.printf("claude code: %v\n", err)
		return 1
	}
	next, err := applyDropKeep(d, cur)
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

// flattenEnv returns m plus every env variable as a top-level entry, so env keys look up like any other.
func flattenEnv(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := maps.Clone(m)
	var env map[string]json.RawMessage
	if json.Unmarshal(m["env"], &env) != nil {
		return out
	}
	for k, v := range env {
		if _, clash := out[k]; !clash {
			out[k] = v
		}
	}
	return out
}

// pageSections builds the page rows: the section keys, then keys only in my defaults, then the actions.
// fields lists the editable rows in order; the action rows follow them.
func pageSections(sections []settings.Section, tmpl, cur, live map[string]json.RawMessage) ([]tui.Section, []settings.Field, map[string]bool) {
	var out []tui.Section
	var fields []settings.Field
	shown := map[string]bool{}
	for _, sec := range sections {
		var rows [][2]string
		for _, key := range sec.Keys {
			shown[key] = true
			f, ok := settings.FieldFor(key)
			if !ok {
				continue
			}
			rows = append(rows, settingRow(f, tmpl, cur, live))
			fields = append(fields, f)
		}
		if len(rows) > 0 {
			out = append(out, tui.Section{Title: "== " + sec.Name + " ==", Rows: rows})
		}
	}
	var rows [][2]string
	for _, key := range slices.Sorted(maps.Keys(cur)) {
		if f, ok := settings.FieldFor(key); ok && !shown[key] {
			rows = append(rows, settingRow(f, tmpl, cur, live))
			fields = append(fields, f)
		}
	}
	if len(rows) > 0 {
		out = append(out, tui.Section{Title: "== added ==", Rows: rows})
	}
	actions := [][2]string{{"+ add a setting", "pick any Claude Code setting"}, {"Save to repo defaults", "write staged keys to the repo file"}}
	return append(out, tui.Section{Title: "== actions ==", Rows: actions}), fields, shown
}

// runClaudeSettingsMenu is the interactive page: rows grouped under Sections() headers, edits staged until
// the page is left (Esc or the trailing "Save as default" row), then one confirm writes the overlay.
func runClaudeSettingsMenu(d *dialog, ov map[string]json.RawMessage, sections []settings.Section) bool {
	staged := map[string]json.RawMessage{} // explicit new overlay value for this key, this page
	reset := map[string]bool{}             // key removed from the overlay (back to the default), this page
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
		tmpl, live = flattenEnv(tmpl), flattenEnv(live)
		cur := maps.Clone(ov)
		for k := range reset {
			delete(cur, k)
		}
		maps.Copy(cur, staged)

		tuiSections, fields, shown := pageSections(sections, tmpl, cur, live)
		idx, err := pickSettingsPage("claude code — your defaults", tuiSections, enterRow("Save as default"), start)
		if err != nil || idx < 0 || idx > len(fields)+1 {
			return true
		}
		start = idx
		switch {
		case idx == len(fields):
			for k := range cur {
				shown[k] = true
			}
			if key, raw, ok := addSetting(d, shown); ok {
				stageSetting(ov, staged, reset, key, raw, tmpl[key])
			}
		case idx == len(fields)+1:
			saveRepoDefaults(d, staged)
		default:
			f := fields[idx]
			if raw, ok := editSetting(d, f, tmpl[f.Key]); ok {
				stageSetting(ov, staged, reset, f.Key, raw, tmpl[f.Key])
			}
		}
	}
}

// addSetting picks a schema key not already shown, then its value; ok is false on cancel or a bad value.
func addSetting(d *dialog, shown map[string]bool) (key string, raw json.RawMessage, ok bool) {
	var cand []settings.Key
	for _, k := range settings.SchemaKeys() {
		if !shown[k.Name] && k.Kind != settings.KindObject && k.Kind != settings.KindArray && k.Name != "$schema" && !settings.CredentialKey(k.Name) {
			cand = append(cand, k)
		}
	}
	key = pickSchemaKey(d, cand)
	f, found := settings.FieldFor(key)
	if !found {
		return "", nil, false
	}
	raw, ok = editSetting(d, f, nil)
	return key, raw, ok && raw != nil
}

// pickSchemaKey runs the filterable list (key + description, 80 columns), else a typed name; "" is a cancel.
func pickSchemaKey(d *dialog, cand []settings.Key) string {
	width := 0
	for _, k := range cand {
		width = max(width, len([]rune(k.Name)))
	}
	room := 80 - 4 - width // marker, label, two-space gap
	rows := make([]tui.ListRow, len(cand))
	for i, k := range cand {
		rows[i] = tui.ListRow{Label: k.Name, Detail: textutil.Truncate(strings.Join(strings.Fields(k.Description), " "), max(room, 0))}
	}
	res, err := addSettingList(tui.ListSpec{Title: "add a setting (type to filter)", Rows: rows, Search: true}, 0)
	if err == nil {
		if res.Index < 0 || res.Index >= len(cand) {
			return ""
		}
		return cand[res.Index].Name
	}
	name := strings.TrimSpace(textLine(d, "setting name: "))
	if !slices.ContainsFunc(cand, func(k settings.Key) bool { return k.Name == name }) {
		if name != "" {
			d.printf("claude code: %s: not an addable setting\n", name)
		}
		return ""
	}
	return name
}

// saveRepoDefaults writes the staged keys into the repo defaults file and prints the git diff --stat; no commit.
func saveRepoDefaults(d *dialog, staged map[string]json.RawMessage) {
	if len(staged) == 0 {
		d.println("claude code: nothing staged for the repo defaults")
		return
	}
	if !saveConfirmed(fmt.Sprintf("Write %d key(s) to the repo defaults", len(staged))) {
		return
	}
	path, err := settings.WriteRepoDefaults(staged)
	if err != nil {
		d.printf("claude code: %v\n", err)
		return
	}
	d.printf("claude code: wrote %s (not committed)\n", path)
	stat, err := gitDiffStat(paths.ConfigDir())
	if err != nil {
		d.printf("claude code: git diff --stat: %v\n", err)
	}
	d.println(strings.TrimRight(stat, "\n"))
}

// stageSetting records one edit: nil (explicit remove) or a value matching the repo default both remove the
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
	if !confirmLeave(n, "setting(s) as default") {
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
	d.printf("claude code: %d setting(s) saved as default\n", n)
}
