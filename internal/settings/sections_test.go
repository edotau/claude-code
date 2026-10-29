package settings

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/defaults"
)

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
