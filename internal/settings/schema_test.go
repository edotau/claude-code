package settings

import (
	"encoding/json"
	"testing"
)

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
