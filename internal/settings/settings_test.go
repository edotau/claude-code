package settings

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/defaults"
	"github.com/edotau/claude-code/internal/paths"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRenderGolden(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "settings.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("render drifted from golden (go test -run TestRenderGolden -update):\n%s", got)
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
      {"type": "command", "command": "orca-hook && echo '<done>'", "timeout": 10},
      {"type": "command", "command": "$HOME/.claude/bin/claude-code hook retired-hook", "timeout": 5}
    ]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "foreign-end"}]}]
  },
  "autoUpdates": false
}`

func TestMergePreservesForeign(t *testing.T) {
	rendered, _ := Render()
	merged, sum, err := Merge([]byte(existing), rendered, nil)
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
	if doc.Env["KEEP_ME"] != "1" || doc.Env["CLAUDE_CODE_SCROLL_SPEED"] == "" {
		t.Errorf("env not unioned: %v", doc.Env)
	}
	if doc.Tips == nil || *doc.Tips || doc.Output != "Concise" {
		t.Error("foreign top-level keys dropped")
	}
	if !strings.Contains(string(doc.Permissions["additionalDirectories"]), "/x") || !strings.Contains(string(doc.Permissions["allow"]), "Bash(ls:*)") {
		t.Errorf("permissions not merged: %s", merged)
	}
	if strings.Count(string(doc.Permissions["deny"]), "Read(~/.ssh/**)") != 1 {
		t.Error("deny list duplicated an existing rule")
	}
	cmds := map[string][]string{}
	for ev, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				cmds[ev] = append(cmds[ev], h.Command)
			}
		}
	}
	stop := strings.Join(cmds["Stop"], "\n")
	if !strings.Contains(stop, "orca-hook && echo '<done>'") || strings.Contains(stop, "retired-hook") || !strings.Contains(stop, "hook Stop") {
		t.Errorf("Stop hooks wrong: %q", cmds["Stop"])
	}
	// PreToolUse carries the dispatcher entry plus the safety gate's own entry.
	wantPre, claudeHooks := 0, 0
	for ev, groups := range Events() {
		for _, g := range groups {
			claudeHooks += len(g.Hooks)
			if ev == EventPreToolUse {
				wantPre += len(g.Hooks)
			}
		}
	}
	if len(cmds["SessionEnd"]) != 2 || cmds["SessionEnd"][1] != "foreign-end" || len(cmds["PreToolUse"]) != wantPre {
		t.Errorf("hooks: %v", cmds)
	}
	if strings.Contains(string(merged), "\\u0026") {
		t.Error("merge HTML-escaped a foreign command")
	}
	if sum.ForeignHooks != 2 || sum.HarnessHooks != claudeHooks {
		t.Errorf("summary %+v", sum)
	}
	if !strings.HasPrefix(strings.Join(sum.Kept, ","), "companyAnnouncements,autoUpdates") {
		t.Errorf("kept %v", sum.Kept)
	}
	again, _, err := Merge(merged, rendered, nil)
	if err != nil || string(again) != string(merged) {
		t.Errorf("merge is not idempotent:\n%s", again)
	}
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
	merged, _, err := Merge(nil, rendered, nil)
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
	for _, b := range []string{first, second} {
		if !strings.HasPrefix(b, filepath.Join(paths.CacheDir(), "backups")) {
			t.Errorf("backup %q, want it under CacheDir()/backups", b)
		}
	}
	siblings, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 1 {
		t.Errorf("files beside dest = %v, want only settings.json itself", siblings)
	}
}

func TestMergeKeepsForeignKeysInOrder(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	in := `{"companyAnnouncements":["hi"],"zzForeign":{"a":1}}`
	out, sum, err := Merge([]byte(in), rendered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sum.Kept, ",") != "companyAnnouncements,zzForeign" {
		t.Errorf("kept = %v (want foreign keys in order)", sum.Kept)
	}
	if i, j := strings.Index(string(out), "companyAnnouncements"), strings.Index(string(out), "zzForeign"); i < 0 || j < i {
		t.Errorf("foreign key order lost:\n%s", out)
	}
	again, _, err := Merge(out, rendered, nil)
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

// TestMergeDropsListedForeignKey: a dropped foreign key is removed and recorded; a template key in the list stays.
func TestMergeDropsListedForeignKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	in := `{"skipDangerousModePermissionPrompt":true,"keepMe":1,"env":{"A":"b"}}`
	out, sum, err := Merge([]byte(in), rendered, []string{"skipDangerousModePermissionPrompt", "env"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["skipDangerousModePermissionPrompt"]; ok {
		t.Error("dropped foreign key survived Merge")
	}
	if _, ok := doc["keepMe"]; !ok {
		t.Error("unlisted foreign key was removed")
	}
	if _, ok := doc["env"]; !ok {
		t.Error("template-owned key in the drop list must still be rendered")
	}
	if !slices.Contains(sum.Dropped, "skipDangerousModePermissionPrompt") {
		t.Errorf("Summary.Dropped = %v, want it to list the foreign key", sum.Dropped)
	}
}

func TestMergeKeepsRuntimeKeysWithoutReportingThem(t *testing.T) {
	rendered, _ := Render()
	live := `{"modelSettings":{"claude-fable-5-1":{"effortLevel":"high"}},"someForeign":1}`
	merged, sum, err := Merge([]byte(live), rendered, nil)
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

// A rule retired from the template leaves an existing file; a user's own rule and foreign keys stay.
func TestMergeDropsRetiredPermissionsKeepsForeign(t *testing.T) {
	existing := []byte(`{"enabledPlugins":{"x@y":true},"permissions":{"ask":["Bash(npm *)","Bash(my-tool *)","Bash(git push -f origin main:*)"]}}`)
	rendered := []byte(`{"permissions":{"ask":["Bash(npm publish:*)"]}}`)
	out, _, err := Merge(existing, rendered, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
		Permissions    struct {
			Ask []string `json:"ask"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bash(npm publish:*)", "Bash(my-tool *)"}
	if !slices.Equal(got.Permissions.Ask, want) || !got.EnabledPlugins["x@y"] {
		t.Fatalf("merged = %s, want ask %v and enabledPlugins kept", out, want)
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
	for _, f := range Catalog {
		if f.Section != "" && !have[f.Key] {
			t.Errorf("catalog key %q has no page row", f.Key)
		}
	}
}

// TestDocsTemplateExampleSettings keeps docs/templates/examples/settings/settings.json identical to
// testdata/settings.golden.json — same render, a docs-discoverable location. Rewrite with the package's
// existing -update flag (declared in settings_test.go): `go test ./internal/settings -run
// TestDocsTemplateExampleSettings -update`.
func TestDocsTemplateExampleSettings(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "settings.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	example := filepath.Join("..", "..", "docs", "templates", "examples", "settings", "settings.json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(example), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(example, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("docs/templates/examples/settings/settings.json drifted from testdata/settings.golden.json " +
			"(go test ./internal/settings -run TestDocsTemplateExampleSettings -update)")
	}
}
