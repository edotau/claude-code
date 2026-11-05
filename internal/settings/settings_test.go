package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/defaults"
	"github.com/edotau/claude-code/internal/paths"
)

func TestRenderValidJSON(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(got) {
		t.Error("render is not valid JSON")
	}
}

const existing = `{
  "env": {"KEEP_ME": "1"},
  "companyAnnouncements": ["hi"],
  "permissions": {"allow": ["Bash(ls:*)"], "deny": ["Read(~/.ssh/**)"], "additionalDirectories": ["/x"]},
  "statusLine": {"type": "command", "command": "mine"},
  "hooks": {
    "Stop": [{"hooks": [
      {"type": "command", "command": "foreign-hook && echo '<done>'", "timeout": 10},
      {"type": "command", "command": "$HOME/.claude/bin/claude-code hook retired-hook", "timeout": 5}
    ]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "foreign-end"}]}]
  },
  "autoUpdates": false
}`

func TestMergePreservesForeign(t *testing.T) {
	rendered, _ := Render()
	merged, sum, err := Merge([]byte(existing), rendered)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env         map[string]string          `json:"env"`
		Tips        *bool                      `json:"spinnerTipsEnabled"`
		Permissions map[string]json.RawMessage `json:"permissions"`
		StatusLine  map[string]any             `json:"statusLine"`
		Hooks       map[string][]MatcherGroup  `json:"hooks"`
		Output      string                     `json:"outputStyle"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("%v\n%s", err, merged)
	}
	if len(doc.Env) != 1 || doc.Env["KEEP_ME"] != "1" {
		t.Errorf("env was changed: %v", doc.Env)
	}
	if doc.Tips != nil || doc.Output != "" {
		t.Error("a template key the user lacks was added")
	}
	var allow []string
	_ = json.Unmarshal(doc.Permissions["allow"], &allow)
	if len(doc.Permissions) != 3 || !slices.Equal(allow, []string{"Bash(ls:*)"}) {
		t.Errorf("permissions were changed: %s", merged)
	}
	checkMergedHooks(t, doc.Hooks, merged, sum)
	checkMergeSummary(t, sum)
	checkMergeIdempotent(t, merged, rendered)
}

func checkMergeIdempotent(t *testing.T, merged, rendered []byte) {
	t.Helper()
	again, _, err := Merge(merged, rendered)
	if err != nil || string(again) != string(merged) {
		t.Errorf("merge is not idempotent:\n%s", again)
	}
}

func checkMergedHooks(t *testing.T, hooks map[string][]MatcherGroup, merged []byte, sum Summary) {
	t.Helper()
	cmds := hookCommands(hooks)
	stop := strings.Join(cmds["Stop"], "\n")
	if !strings.Contains(stop, "foreign-hook && echo '<done>'") || strings.Contains(stop, "retired-hook") || !strings.Contains(stop, "hook Stop") {
		t.Errorf("Stop hooks wrong: %q", cmds["Stop"])
	}
	// PreToolUse carries the dispatcher entry plus the safety gate's own entry.
	wantPre, claudeHooks := renderedHookCounts()
	if len(cmds["SessionEnd"]) != 2 || cmds["SessionEnd"][1] != "foreign-end" || len(cmds["PreToolUse"]) != wantPre {
		t.Errorf("hooks: %v", cmds)
	}
	if strings.Contains(string(merged), "\\u0026") {
		t.Error("merge HTML-escaped a foreign command")
	}
	if sum.ForeignHooks != 2 || sum.HarnessHooks != claudeHooks {
		t.Errorf("summary %+v", sum)
	}
}

func checkMergeSummary(t *testing.T, sum Summary) {
	t.Helper()
	if !strings.HasPrefix(strings.Join(sum.Kept, ","), "companyAnnouncements,autoUpdates") {
		t.Errorf("kept %v", sum.Kept)
	}
}

func hookCommands(hooks map[string][]MatcherGroup) map[string][]string {
	cmds := map[string][]string{}
	for ev, groups := range hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				cmds[ev] = append(cmds[ev], h.Command)
			}
		}
	}
	return cmds
}

func renderedHookCounts() (wantPre, claudeHooks int) {
	for ev, groups := range Events() {
		for _, g := range groups {
			claudeHooks += len(g.Hooks)
			if ev == EventPreToolUse {
				wantPre += len(g.Hooks)
			}
		}
	}
	return wantPre, claudeHooks
}

// hermeticCacheDir isolates CLAUDE_CONFIG_DIR so WriteWithBackup's paths.BackupPath never touches
// the real ~/.claude/state/harness.
func hermeticCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

func TestMergeEmptyAndBackup(t *testing.T) {
	hermeticCacheDir(t)
	rendered, _ := Render()
	merged, _, err := Merge(nil, rendered)
	if err != nil || string(merged) != string(rendered) {
		t.Fatalf("merge onto nothing should equal the render: %v", err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	if b, err := WriteWithBackup(file, []byte("{}\n")); err != nil || b != "" {
		t.Fatalf("fresh write: %q %v", b, err)
	}
	beforeSiblings, _ := os.ReadDir(dir)
	b, err := WriteWithBackup(file, merged)
	if err != nil || b == "" {
		t.Fatalf("backup: %q %v", b, err)
	}
	if old, _ := os.ReadFile(b); string(old) != "{}\n" {
		t.Errorf("backup content %q", old)
	}
	if !strings.HasPrefix(b, filepath.Join(paths.CacheDir(), "backups")) {
		t.Errorf("backup %q, want it under CacheDir()/backups", b)
	}
	afterSiblings, _ := os.ReadDir(dir)
	if len(afterSiblings) != len(beforeSiblings) {
		t.Errorf("files beside dest changed: before %v, after %v", beforeSiblings, afterSiblings)
	}
}

func TestWriteWithBackupPreservesBackToBackBackups(t *testing.T) {
	hermeticCacheDir(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	original := []byte("{\"original\":true}\n")
	if err := os.WriteFile(file, original, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := WriteWithBackup(file, []byte("{\"updated\":true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := WriteWithBackup(file, []byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Errorf("back-to-back writes reused backup path %q", first)
	}
	got, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("first backup = %q, want %q", got, original)
	}
	checkBackupLocations(t, first, second)
	siblings, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 2 { // settings.json and its advisory lock file
		t.Errorf("files beside dest = %v, want only settings.json and its .lock", siblings)
	}
}

func checkBackupLocations(t *testing.T, backups ...string) {
	t.Helper()
	for _, b := range backups {
		if !strings.HasPrefix(b, filepath.Join(paths.CacheDir(), "backups")) {
			t.Errorf("backup %q, want it under CacheDir()/backups", b)
		}
	}
}

func TestMergeKeepsForeignKeysInOrder(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	in := `{"companyAnnouncements":["hi"],"zzForeign":{"a":1}}`
	out, sum, err := Merge([]byte(in), rendered)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sum.Kept, ",") != "companyAnnouncements,zzForeign" {
		t.Errorf("kept = %v (want foreign keys in order)", sum.Kept)
	}
	if i, j := strings.Index(string(out), "companyAnnouncements"), strings.Index(string(out), "zzForeign"); i < 0 || j < i {
		t.Errorf("foreign key order lost:\n%s", out)
	}
	again, _, err := Merge(out, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Error("merge is not idempotent")
	}
}

func TestMergeHooksPreservesForeignFieldsAndOrder(t *testing.T) {
	base := `{"Stop":[{"matcher":"x","extra":{"keep":true},"hooks":[
		{"custom":1,"type":"command","command":"first","tail":false},
		{"command":"$HOME/.claude/bin/claude-code hook retired-hook"},
		{"type":"prompt","prompt":"keep this","custom":2}
	],"timeout":9},{"hooks":[{"command":"last"}],"tail":"end"}]}`
	want := `{"Stop":[{"matcher":"x","extra":{"keep":true},"hooks":[{"custom":1,"type":"command","command":"first","tail":false},{"type":"prompt","prompt":"keep this","custom":2}],"timeout":9},{"hooks":[{"command":"last"}],"tail":"end"}]}`
	var sum Summary
	got, err := mergeHooks(json.RawMessage(base), json.RawMessage(`{}`), &sum)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, got); err != nil {
		t.Fatal(err)
	}
	if compact.String() != want || sum.ForeignHooks != 3 || sum.HarnessHooks != 0 {
		t.Fatalf("mergeHooks = %s, %+v", got, sum)
	}
}

// A rendered file carrying the stale `rtk hook claude` entry loses it; a real foreign hook stays.
func TestMergeDropsStaleRTKHookKeepsForeign(t *testing.T) {
	base := `{"PreToolUse":[{"matcher":"Bash","hooks":[
		{"type":"command","command":"/Users/x/.claude/bin/rtk hook claude"},
		{"type":"command","command":"rtk hook claude"},
		{"type":"command","command":"my-linter --check"}
	]}]}`
	var sum Summary
	got, err := mergeHooks(json.RawMessage(base), json.RawMessage(`{}`), &sum)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "rtk hook claude") || !strings.Contains(string(got), "my-linter --check") || sum.ForeignHooks != 1 {
		t.Fatalf("mergeHooks = %s, %+v", got, sum)
	}
}

func TestMergeKeepsRuntimeKeysWithoutReportingThem(t *testing.T) {
	rendered, _ := Render()
	live := `{"modelSettings":{"claude-fable-5-1":{"effortLevel":"high"}},"someForeign":1}`
	merged, sum, err := Merge([]byte(live), rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), `"effortLevel": "high"`) && !strings.Contains(string(merged), `"effortLevel":"high"`) {
		t.Errorf("modelSettings not kept:\n%s", merged)
	}
	if slices.Contains(sum.Kept, "modelSettings") {
		t.Errorf("runtime key reported as foreign: %v", sum.Kept)
	}
	if !slices.Contains(sum.Kept, "someForeign") {
		t.Errorf("a real foreign key must still be reported: %v", sum.Kept)
	}
}

func TestAuthHeaderRejected(t *testing.T) {
	for _, v := range []string{
		"Authorization: Basic dXNlcjpwYXNz",
		"X-Foo: 1\nauthorization: Basic dXNlcjpwYXNz",
		`X-Foo: 1\nCookie: a=b`,
		"x-api-key: abc",
		"Proxy-Authorization: Basic Zm9v",
		"api-key: abc",
	} {
		raw, _ := json.Marshal(v)
		if err := checkNotCredential("ANTHROPIC_CUSTOM_HEADERS", raw); err == nil || !strings.Contains(err.Error(), "vault set") {
			t.Errorf("%q accepted: %v", v, err)
		}
	}
	ok, _ := json.Marshal("X-Team: platform\nX-Trace: 1")
	if err := checkNotCredential("ANTHROPIC_CUSTOM_HEADERS", ok); err != nil {
		t.Errorf("benign header rejected: %v", err)
	}
}

func TestAutoMemoryDirectoryRejected(t *testing.T) {
	ov := map[string]json.RawMessage{"autoMemoryDirectory": json.RawMessage(`"/tmp/x"`)}
	if err := ValidateOverlay(ov); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Errorf("overlay accepted autoMemoryDirectory: %v", err)
	}
	if _, err := applyOverlay([]byte(`{}`), ov); err == nil {
		t.Error("applyOverlay accepted autoMemoryDirectory")
	}
	if !StructuralKey("autoMemoryDirectory") {
		t.Error("picker would offer autoMemoryDirectory")
	}
}

func TestAdditionalPropertiesSchema(t *testing.T) {
	k, ok := SchemaKey("enabledPlugins")
	if !ok {
		t.Skip("no enabledPlugins in schema")
	}
	if err := k.Validate(json.RawMessage(`{"plugin@marketplace":7}`)); err == nil {
		t.Error("non-boolean plugin value accepted")
	}
	if err := k.Validate(json.RawMessage(`{"plugin@marketplace":true}`)); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
}

// TestRenderSchemaFirst: $schema must stay the first key emitted — install/upgrade tooling and humans
// reading settings.json expect it leading the file.
func TestRenderSchemaFirst(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(got))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("render did not start with an object: %v %v", tok, err)
	}
	tok, err := dec.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "$schema" {
		t.Errorf("first key = %q, want $schema", tok)
	}
}

// TestSectionsCoverRenderedKeys: every top-level key Render emits from the section files (all but
// "hooks", which hookspec contributes) must come from exactly one Section — never zero, never two.
func TestSectionsCoverRenderedKeys(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &obj); err != nil {
		t.Fatal(err)
	}
	sections, err := Sections()
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string][]string{}
	for _, s := range sections {
		for _, k := range s.Keys {
			owner[k] = append(owner[k], s.Name)
		}
	}
	keys := slices.Collect(maps.Keys(obj))
	var env map[string]json.RawMessage
	_ = json.Unmarshal(obj["env"], &env)
	for e := range env {
		keys = append(keys, e)
	}
	checkRenderedKeyOwners(t, keys, owner)
	checkSectionKeys(t, owner, obj, env)
}

func checkRenderedKeyOwners(t *testing.T, keys []string, owner map[string][]string) {
	t.Helper()
	for _, k := range keys {
		switch k {
		case "hooks", "env", "permissions", "statusLine": // generated, or an object the page cannot edit
			continue
		case "$schema": // Sections() deliberately excludes it (schema metadata, not a setting)
			continue
		}
		switch owners := owner[k]; len(owners) {
		case 0:
			t.Errorf("rendered key %q is not owned by any Section", k)
		case 1:
			// exactly one owner, as required
		default:
			t.Errorf("rendered key %q owned by %d sections: %v", k, len(owners), owners)
		}
	}
}

func checkSectionKeys(t *testing.T, owner map[string][]string, obj, env map[string]json.RawMessage) {
	t.Helper()
	for k, owners := range owner {
		if len(owners) > 1 {
			t.Errorf("section key %q duplicated across sections: %v", k, owners)
		}
		_, rendered := obj[k]
		_, inEnv := env[k]
		if _, cat := Lookup(k); !rendered && !inEnv && !cat {
			t.Errorf("section %v declares key %q that Render never emits and the catalog lacks", owners, k)
		}
	}
}

// The env block holds only env-only variables: one the schema documents as a settings key's override belongs in that key.
func TestEnvHoldsOnlyEnvOnlyVariables(t *testing.T) {
	o, err := parseObject(defaults.Claude)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(o.vals["env"], &env); err != nil {
		t.Fatal(err)
	}
	for name := range env {
		for _, k := range SchemaKeys() {
			if !k.Env && k.Name != "env" && strings.Contains(k.Description, name) {
				t.Errorf("env %s has a settings key (%s): set the key in the defaults file instead", name, k.Name)
			}
		}
	}
}

// TestDefaultsMatchSchema: every key in the defaults file passes its schema validation, or is a docs-sourced catalog key.
func TestDefaultsMatchSchema(t *testing.T) {
	o, err := parseObject(defaults.Claude)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range o.keys {
		checkDefault(t, k, o.vals[k])
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(o.vals["env"], &env); err != nil {
		t.Fatal(err)
	}
	for k, v := range env {
		checkDefault(t, k, v)
	}
}

func checkDefault(t *testing.T, name string, raw json.RawMessage) {
	t.Helper()
	if f, ok := Lookup(name); ok && f.Source == "docs" {
		return
	}
	k, ok := SchemaKey(name)
	if !ok {
		t.Errorf("default key %q is not in the schema or a docs catalog entry", name)
		return
	}
	if err := k.Validate(raw); err != nil {
		t.Errorf("default key %q: %v", name, err)
	}
}

// TestSchemaKeyValidateRejects: an unknown key is absent and a wrong type or out-of-enum value fails.
func TestSchemaKeyValidateRejects(t *testing.T) {
	if _, ok := SchemaKey("noSuchSetting"); ok {
		t.Error("SchemaKey(noSuchSetting) found a key")
	}
	cases := map[string]string{"tui": `"sideways"`, "cleanupPeriodDays": `"30"`, "prefersReducedMotion": `1`, "permissions": `[]`}
	for name, raw := range cases {
		k, ok := SchemaKey(name)
		if !ok {
			t.Fatalf("%s missing from schema", name)
		}
		if k.Validate(json.RawMessage(raw)) == nil {
			t.Errorf("%s accepted %s", name, raw)
		}
	}
	if k, _ := SchemaKey("theme"); k.Validate(json.RawMessage(`"custom:mine"`)) != nil {
		t.Error("theme rejected custom:mine")
	}
	if k, _ := SchemaKey("CLAUDE_CODE_SCROLL_SPEED"); !k.Env {
		t.Error("CLAUDE_CODE_SCROLL_SPEED not an env key")
	}
}

// A catalog key absent from the defaults file still has a page row, so removing it from the file loses nothing.
func TestSectionsKeepCatalogKeysMissingFromDefaults(t *testing.T) {
	orig := defaults.Claude
	t.Cleanup(func() { defaults.Claude = orig })
	defaults.Claude = []byte(`{"$schema": "x", "model": "opus"}`)
	sections, err := Sections()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, s := range sections {
		for _, k := range s.Keys {
			have[k] = true
		}
	}
	for _, f := range Catalog() {
		if f.Section != "" && !have[f.Key] {
			t.Errorf("catalog key %q has no page row", f.Key)
		}
	}
}

func TestUpdateWithBackupMergesFromLockedRead(t *testing.T) {
	hermeticCacheDir(t)
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(file, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen string
	b, err := UpdateWithBackup(file, func(old []byte) ([]byte, error) { seen = string(old); return []byte("new"), nil })
	got, _ := os.ReadFile(file)
	if err != nil || seen != "old" || string(got) != "new" || b == "" {
		t.Fatalf("seen=%q file=%q backup=%q err=%v", seen, got, b, err)
	}
}

func TestKeyValidateRefusesUncompiledPattern(t *testing.T) {
	k := Key{Name: "x", Kind: KindString, patternErr: errors.New("bad")}
	if err := k.Validate(json.RawMessage(`"a"`)); err == nil {
		t.Error("a key whose pattern failed to compile validated a value")
	}
}

// UpdateWithBackup holds the file lock across read-merge-write, so concurrent writers' keys all survive.
func TestUpdateWithBackupKeepsConcurrentKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, err := UpdateWithBackup(path, func(old []byte) ([]byte, error) {
				var m map[string]int
				if err := json.Unmarshal(old, &m); err != nil {
					return nil, err
				}
				time.Sleep(2 * time.Millisecond)
				m[fmt.Sprintf("k%d", i)] = i
				return json.Marshal(m)
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	b, _ := os.ReadFile(path)
	var got map[string]int
	if err := json.Unmarshal(b, &got); err != nil || len(got) != n {
		t.Errorf("settings keys = %v (%v), want %d", got, err, n)
	}
}
