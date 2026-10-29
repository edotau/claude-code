package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCredentialKeyRefusedEverywhere(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "MCP_CLIENT_SECRET", "apiKeyHelper", "awsCredentialExport"} {
		if !CredentialKey(k) {
			t.Errorf("CredentialKey(%q) = false", k)
		}
	}
	for _, k := range []string{"MAX_THINKING_TOKENS", "CLAUDE_CODE_API_KEY_HELPER_TTL_MS", "prefersReducedMotion", "theme"} {
		if CredentialKey(k) {
			t.Errorf("CredentialKey(%q) = true", k)
		}
	}
	dir := vaulttest.Use(t)
	file := filepath.Join(dir, filepath.FromSlash(RepoDefaultsRel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{\n  \"$schema\": \"x\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, ov := range []map[string]json.RawMessage{
		{"ANTHROPIC_API_KEY": json.RawMessage(`"abc"`)},
		{"apiKeyHelper": json.RawMessage(`"get-key.sh"`)},
		{"outputStyle": json.RawMessage(`"sk-live-abcdefghijklmnop"`)},
	} {
		if err := SaveOverlay(ov); err == nil || !strings.Contains(err.Error(), "vault set") {
			t.Errorf("SaveOverlay(%v) = %v, want a vault-set refusal", ov, err)
		}
		if _, err := WriteRepoDefaults(ov); err == nil || !strings.Contains(err.Error(), "vault set") {
			t.Errorf("WriteRepoDefaults(%v) = %v, want a vault-set refusal", ov, err)
		}
	}
	if got, _ := os.ReadFile(file); strings.Contains(string(got), "KEY") {
		t.Errorf("credential reached the repo defaults:\n%s", got)
	}
}

func TestSchemaNumbersBoundsAndUnions(t *testing.T) {
	ok := func(key, val string, want bool) {
		t.Helper()
		k, found := SchemaKey(key)
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
	ok("theme", `"custom:mine"`, true)
	ok("theme", `"custom:"`, false)

	f, found := FieldFor("feedbackSurveyRate")
	if !found {
		t.Fatal("no field")
	}
	if raw, err := f.Parse("0.25"); err != nil || string(raw) != "0.25" {
		t.Errorf("Parse(0.25) = %s, %v", raw, err)
	}
	if _, err := f.Parse("abc"); err == nil {
		t.Error("Parse(abc) accepted")
	}
	if _, err := f.Parse("7"); err == nil {
		t.Error("Parse(7) accepted past the maximum")
	}
}
