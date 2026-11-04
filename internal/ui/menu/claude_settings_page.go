package menu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// pickSettingsPage is the sectioned picker seam; tests replace it.
var pickSettingsPage = tui.PickSections

// addSettingList is the filterable "+ add a setting" picker seam; tests replace it.
var addSettingList = tui.List

// gitDiffStat prints the uncommitted change to the repo defaults file; tests replace it.
var gitDiffStat = func(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--stat", "--", settings.RepoDefaultsRel).CombinedOutput()
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
	return homeRow{Name: "claude code", State: detail, Check: "every claude launch", Run: func(d *dialog) { runClaudeSettings(d) }}
}

// configDirKey is the env section's read-only row: writing it would make claude split off a stray .claude.json.
const configDirKey = "CLAUDE_CONFIG_DIR"

// withInstallRow appends the install path to the env section's rows; other sections pass through.
func withInstallRow(section string, rows [][2]string, fields []settings.Field) ([][2]string, []settings.Field) {
	if section != "env" {
		return rows, fields
	}
	return append(rows, [2]string{configDirKey, paths.ConfigDir() + "  install"}), append(fields, settings.Field{Key: configDirKey})
}

// settingRow renders one catalog field as a page row: key, then its effective value and source
// (default|mine), flagging when the live settings.json holds something else. tmpl/ov/live are
// parsed top-level settings objects; a missing live map (no file yet) never raises the flag.
func settingRow(f settings.Field, tmpl, ov, live map[string]json.RawMessage) [2]string {
	eff, _ := lookupKey(tmpl, f.Key)
	source := "default"
	if v, ok := lookupKey(ov, f.Key); ok {
		eff, source = v, "mine"
	}
	if l, ok := lookupKey(live, f.Key); ok && !jsonEqual(l, eff) {
		source += " · live differs"
	}
	return [2]string{f.Key, settingText(eff) + "  " + source}
}

// lookupKey reads key from m, or "parent.sub" from the parent's object when m has no such top-level key.
func lookupKey(m map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	parent, sub, nested := strings.Cut(key, ".")
	var obj map[string]json.RawMessage
	if !nested || json.Unmarshal(m[parent], &obj) != nil {
		return nil, false
	}
	v, ok := obj[sub]
	return v, ok
}

// fieldRows is f's page rows: one row per scalar leaf for an object setting (worktree.baseRef), else its own row.
func fieldRows(f settings.Field, tmpl, cur, live map[string]json.RawMessage) ([][2]string, []settings.Field) {
	eff, ok := cur[f.Key]
	if !ok {
		eff = tmpl[f.Key]
	}
	var obj map[string]json.RawMessage
	if f.Kind != settings.KindObject || json.Unmarshal(eff, &obj) != nil || len(obj) == 0 {
		return [][2]string{settingRow(f, tmpl, cur, live)}, []settings.Field{f}
	}
	var rows [][2]string
	var fields []settings.Field
	for _, sub := range slices.Sorted(maps.Keys(obj)) {
		lf, ok := settings.SubField(f.Key, sub)
		if !ok {
			return [][2]string{settingRow(f, tmpl, cur, live)}, []settings.Field{f}
		}
		rows, fields = append(rows, settingRow(lf, tmpl, cur, live)), append(fields, lf)
	}
	return rows, fields
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
	if f.Key == configDirKey {
		return nil, false
	}
	if f.Kind == settings.KindBool || f.Kind == settings.KindEnum {
		return editChoice(d, f, tmplVal)
	}
	return editText(d, f)
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
	idx, err := d.shell().Pick(f.Key, rows)
	if err != nil || idx < 0 {
		return nil, false
	}
	if idx == resetIdx {
		return nil, true
	}
	if idx == customIdx {
		name := strings.TrimSpace(d.shell().Line("theme custom name: "))
		if name == "" {
			return nil, false
		}
		return parseSettingValue(d, f, "custom:"+name)
	}
	return parseSettingValue(d, f, values[idx])
}

// parseSettingValue parses input for f, printing the parse error as "<key>: <err>" and returning false on failure.
func parseSettingValue(d *dialog, f settings.Field, input string) (json.RawMessage, bool) {
	raw, err := f.Parse(input)
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
func editText(d *dialog, f settings.Field) (json.RawMessage, bool) {
	input := strings.TrimSpace(d.shell().Line(f.Key + " (empty removes): "))
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
// source (default|mine[ · live differs]) — no prompts, no writes.
func reportClaudeSettings(d *dialog, ov map[string]json.RawMessage, sections []settings.Section) bool {
	tmpl, err := configure.RenderObject(settings.Render)
	if err != nil {
		d.printf("claude code: %v\n", err)
		return false
	}
	live, err := configure.ReadLiveSettings()
	if err != nil && !os.IsNotExist(err) {
		d.printf("claude code: %v\n", err)
	}
	rp := &settingsReport{d: d, ov: ov, tmpl: flattenEnv(tmpl), live: flattenEnv(live), ok: true, shown: map[string]bool{}}
	for _, sec := range sections {
		rp.section(sec)
	}
	rp.added()
	if d.o.Check {
		return rp.ok
	}
	return true
}

// settingsReport carries the state of one reportClaudeSettings run: ok turns false when a live value differs.
type settingsReport struct {
	d              *dialog
	ov, tmpl, live map[string]json.RawMessage
	ok             bool
	shown          map[string]bool
}

func (rp *settingsReport) emit(f settings.Field) {
	row := settingRow(f, rp.tmpl, rp.ov, rp.live)
	rp.d.println(row[0] + "  " + row[1])
	if strings.Contains(row[1], "live differs") {
		rp.ok = false
	}
}

// section prints sec's header (once, before its first known key) and one line per key.
func (rp *settingsReport) section(sec settings.Section) {
	header := false
	for _, key := range sec.Keys {
		rp.shown[key] = true
		f, found := settings.FieldFor(key)
		if !found {
			continue
		}
		if !header {
			rp.d.println(Heading(sec.Name))
			header = true
		}
		rp.emit(f)
	}
}

// added prints the overlay keys no section listed, under an "added" header.
func (rp *settingsReport) added() {
	header := false
	for _, key := range slices.Sorted(maps.Keys(rp.ov)) {
		f, found := settings.FieldFor(key)
		if !found || rp.shown[key] {
			continue
		}
		if !header {
			rp.d.println(Heading("added"))
			header = true
		}
		rp.emit(f)
	}
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
			if !ok || key == "$schema" { // $schema is a reference, not a setting; settings.json still gets it
				continue
			}
			r, fs := fieldRows(f, tmpl, cur, live)
			rows, fields = append(rows, r...), append(fields, fs...)
		}
		rows, fields = withInstallRow(sec.Name, rows, fields)
		if len(rows) > 0 {
			out = append(out, tui.Section{Title: Heading(sec.Name), Rows: rows})
		}
	}
	var rows [][2]string
	for _, key := range slices.Sorted(maps.Keys(cur)) {
		if f, ok := settings.FieldFor(key); ok && !shown[key] {
			r, fs := fieldRows(f, tmpl, cur, live)
			rows, fields = append(rows, r...), append(fields, fs...)
		}
	}
	if len(rows) > 0 {
		out = append(out, tui.Section{Title: "== added ==", Rows: rows})
	}
	actions := [][2]string{{"+ add a setting", "pick any Claude Code setting"}, {"Save to repo defaults", "write staged keys to the repo file"}}
	return append(out, tui.Section{Title: Heading("actions"), Rows: actions}), fields, shown
}

// runClaudeSettingsMenu is the interactive page: rows grouped under Sections() headers, edits staged until
// the page is left (Esc or the trailing "Save as default" row), then one confirm writes the overlay.
func runClaudeSettingsMenu(d *dialog, ov map[string]json.RawMessage, sections []settings.Section) bool {
	pg := &claudeSettingsPage{
		d:      d,
		ov:     ov,
		staged: map[string]json.RawMessage{}, // explicit new overlay value for this key, this page
		reset:  map[string]bool{},            // key removed from the overlay (back to the default), this page
	}
	defer saveClaudeSettings(d, pg.staged, pg.reset)
	start := 0
	for {
		tmpl, live, err := pg.loadMaps()
		if err != nil {
			d.printf("claude code: %v\n", err)
			return false
		}
		cur := pg.current()
		tuiSections, fields, shown := pageSections(sections, tmpl, cur, live)
		idx, err := pickSettingsPage("claude code — your defaults", tuiSections, EnterRow("Save as default"), start)
		if err != nil || idx < 0 || idx > len(fields)+1 {
			return true
		}
		start = idx
		pg.act(idx, fields, shown, tmpl, cur)
	}
}

// claudeSettingsPage is the staging state of one runClaudeSettingsMenu session.
type claudeSettingsPage struct {
	d      *dialog
	ov     map[string]json.RawMessage
	staged map[string]json.RawMessage
	reset  map[string]bool
}

// loadMaps re-renders the repo template and re-reads the live settings, both with env flattened; a live read
// error other than not-exist is printed and ignored.
func (pg *claudeSettingsPage) loadMaps() (tmpl, live map[string]json.RawMessage, err error) {
	tmpl, err = configure.RenderObject(settings.Render)
	if err != nil {
		return nil, nil, err
	}
	live, err = configure.ReadLiveSettings()
	if err != nil && !os.IsNotExist(err) {
		pg.d.printf("claude code: %v\n", err)
	}
	return flattenEnv(tmpl), flattenEnv(live), nil
}

// current is the overlay with this page's resets dropped and staged values applied.
func (pg *claudeSettingsPage) current() map[string]json.RawMessage {
	cur := maps.Clone(pg.ov)
	for k := range pg.reset {
		delete(cur, k)
	}
	maps.Copy(cur, pg.staged)
	return cur
}

// act runs the picked row: a setting edit, the "+ add a setting" row (idx == len(fields)) or "Save to repo defaults".
func (pg *claudeSettingsPage) act(idx int, fields []settings.Field, shown map[string]bool, tmpl, cur map[string]json.RawMessage) {
	d := pg.d
	switch {
	case idx == len(fields):
		for k := range cur {
			shown[k] = true
		}
		if key, raw, ok := addSetting(d, shown); ok {
			stageSetting(pg.ov, pg.staged, pg.reset, key, raw, tmpl[key])
		}
	case idx == len(fields)+1:
		saveRepoDefaults(d, pg.staged)
	default:
		f := fields[idx]
		tv, _ := lookupKey(tmpl, f.Key)
		raw, ok := editSetting(d, f, tv)
		if !ok {
			return
		}
		if err := stageField(pg.ov, pg.staged, pg.reset, cur, tmpl, f.Key, raw); err != nil {
			d.printf("claude code: %v\n", err)
		}
	}
}

// addSetting picks a schema key not already shown, then its value; ok is false on cancel or a bad value.
func addSetting(d *dialog, shown map[string]bool) (key string, raw json.RawMessage, ok bool) {
	cand := addableKeys(shown)
	key = pickSchemaKey(d, cand)
	f, found := settings.FieldFor(key)
	if !found {
		return "", nil, false
	}
	raw, ok = editSetting(d, f, nil)
	return key, raw, ok && raw != nil
}

// addableKeys lists the schema keys not yet shown that are plain scalars: no objects, arrays, refs or credentials.
func addableKeys(shown map[string]bool) []settings.Key {
	var cand []settings.Key
	for _, k := range settings.SchemaKeys() {
		if !shown[k.Name] && k.Kind != settings.KindObject && k.Kind != settings.KindArray && k.Name != "$schema" && !settings.CredentialKey(k.Name) && !settings.StructuralKey(k.Name) && !k.HasRef() {
			cand = append(cand, k)
		}
	}
	return cand
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
	name := strings.TrimSpace(d.shell().Line("setting name: "))
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

// stageField stages key, writing a "parent.sub" leaf back into a copy of the parent object (other sub-keys kept).
func stageField(ov, staged map[string]json.RawMessage, reset map[string]bool, cur, tmpl map[string]json.RawMessage, key string, raw json.RawMessage) error {
	parent, sub, nested := strings.Cut(key, ".")
	if _, top := tmpl[key]; !nested || top {
		stageSetting(ov, staged, reset, key, raw, tmpl[key])
		return nil
	}
	base, ok := cur[parent]
	if !ok {
		base = tmpl[parent]
	}
	next := map[string]json.RawMessage{}
	if len(base) > 0 {
		if err := json.Unmarshal(base, &next); err != nil || next == nil {
			return fmt.Errorf("%s is not a JSON object; edit %s in the vault instead", parent, key)
		}
	}
	if raw == nil { // remove from mine: back to the repo default's sub-value
		raw, _ = lookupKey(tmpl, key)
	}
	if raw == nil {
		delete(next, sub)
	} else {
		next[sub] = raw
	}
	b, _ := json.Marshal(next)
	stageSetting(ov, staged, reset, parent, b, tmpl[parent])
	return nil
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
// overlay is re-read inside the write transaction, so only this session's staged sets/resets are applied —
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
	err := settings.UpdateOverlay(func(cur map[string]json.RawMessage) map[string]json.RawMessage {
		next := maps.Clone(cur)
		for k := range reset {
			delete(next, k)
		}
		maps.Copy(next, staged)
		return next
	})
	if err != nil {
		d.printf("claude code: %v\n", err)
		return
	}
	d.changed = true
	d.printf("claude code: %d setting(s) saved as default\n", n)
}
