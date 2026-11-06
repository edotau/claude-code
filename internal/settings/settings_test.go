package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
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

func TestAdditionalPropertiesSchema(t *testing.T) {
	k, ok := schemaKey("enabledPlugins")
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

// TestSchemaKeyValidateRejects: an unknown key is absent and a wrong type or out-of-enum value fails.
func TestSchemaKeyValidateRejects(t *testing.T) {
	if _, ok := schemaKey("noSuchSetting"); ok {
		t.Error("schemaKey(noSuchSetting) found a key")
	}
	cases := map[string]string{"tui": `"sideways"`, "cleanupPeriodDays": `"30"`, "prefersReducedMotion": `1`, "permissions": `[]`}
	for name, raw := range cases {
		k, ok := schemaKey(name)
		if !ok {
			t.Fatalf("%s missing from schema", name)
		}
		if k.Validate(json.RawMessage(raw)) == nil {
			t.Errorf("%s accepted %s", name, raw)
		}
	}
	if k, _ := schemaKey("theme"); k.Validate(json.RawMessage(`"custom:mine"`)) != nil {
		t.Error("theme rejected custom:mine")
	}
	if k, _ := schemaKey("CLAUDE_CODE_SCROLL_SPEED"); !k.Env {
		t.Error("CLAUDE_CODE_SCROLL_SPEED not an env key")
	}
}

func TestKeyValidateRefusesUncompiledPattern(t *testing.T) {
	k := Key{Name: "x", Kind: kindString, patternErr: errors.New("bad")}
	if err := k.Validate(json.RawMessage(`"a"`)); err == nil {
		t.Error("a key whose pattern failed to compile validated a value")
	}
}

func TestSchemaNumbersBoundsAndUnions(t *testing.T) {
	ok := func(key, val string, want bool) {
		t.Helper()
		k, found := schemaKey(key)
		if !found {
			t.Fatalf("no schema key %s", key)
		}
		if err := k.Validate(json.RawMessage(val)); (err == nil) != want {
			t.Errorf("%s=%s: err = %v, want accepted=%v", key, val, err, want)
		}
	}
	ok("feedbackSurveyRate", "0.5", true)
	ok("feedbackSurveyRate", "1.5", false)     // maximum
	ok("skillListingMaxDescChars", "0", false) // exclusiveMinimum
	ok("skillListingMaxDescChars", "5", true)
	ok("skillListingBudgetFraction", "0", false)
	ok("skillListingBudgetFraction", "2", false)
	ok("strictPluginOnlyCustomization", "true", true) // union: boolean branch
	ok("strictPluginOnlyCustomization", `["skills"]`, true)
	ok("strictPluginOnlyCustomization", `"x"`, false)
	ok("forceLoginOrgUUID", `["a","b"]`, true) // oneOf: array branch
	ok("forceLoginOrgUUID", "3", false)
	ok("forceLoginOrgUUID", `[3]`, false) // array items
	ok("strictPluginOnlyCustomization", `["bogus"]`, false)
	ok("strictPluginOnlyCustomization", `["hooks"]`, true)
	ok("env", `{"MAX_THINKING_TOKENS":5}`, false) // nested property
	ok("theme", `"custom:mine"`, true)
	ok("theme", `"custom:"`, false)

}

func TestObjectConstraintsEnforced(t *testing.T) {
	k, _ := schemaKey("subagentStatusLine")
	for val, want := range map[string]bool{
		`{"type":"command","command":"c"}`:           true,
		`{"command":"c"}`:                            false, // required
		`{"type":"other","command":"c"}`:             false, // const
		`{"type":"command","command":"c","extra":1}`: false, // additionalProperties
	} {
		if err := k.Validate(json.RawMessage(val)); (err == nil) != want {
			t.Errorf("subagentStatusLine=%s: err = %v, want accepted=%v", val, err, want)
		}
	}
}

// TestCheckReportsSyntaxLocationAndSchemaErrors: valid passes, unknown keys pass, a bad value or bad JSON is named.
func TestCheckReportsSyntaxLocationAndSchemaErrors(t *testing.T) {
	for in, want := range map[string]string{
		`{"model": "opus", "zzMine": [1]}`:  "",
		`{"cleanupPeriodDays": "7"}`:        "cleanupPeriodDays: want a number",
		"{\n  \"model\": \"x\",\n  oops\n}": "line 3 col 4",
		`[]`:                                "cannot unmarshal array",
		`null`:                              "not a JSON object",
	} {
		err := Check([]byte(in))
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("Check(%q) = %v, want %q", in, err, want)
		}
	}
}
