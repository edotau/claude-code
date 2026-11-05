package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/defaults"
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
		case "hooks", "env", "permissions", "statusLine", "attribution": // generated, or an object the page cannot edit
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

func TestKeyValidateRefusesUncompiledPattern(t *testing.T) {
	k := Key{Name: "x", Kind: KindString, patternErr: errors.New("bad")}
	if err := k.Validate(json.RawMessage(`"a"`)); err == nil {
		t.Error("a key whose pattern failed to compile validated a value")
	}
}
