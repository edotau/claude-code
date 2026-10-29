package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFxAuthHeaderRejected(t *testing.T) {
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

func TestFxAutoMemoryDirectoryRejected(t *testing.T) {
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

func TestFxAdditionalPropertiesSchema(t *testing.T) {
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
