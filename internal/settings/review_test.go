package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		{"env": json.RawMessage(`{"ANTHROPIC_API_KEY":"abc"}`)},
		{"autoMode": json.RawMessage(`{"a":[{"b":"ghp_abcdefghijklmnopqrstuvwxyz0123456789"}]}`)},
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
	ok("forceLoginOrgUUID", `[3]`, false) // array items
	ok("strictPluginOnlyCustomization", `["bogus"]`, false)
	ok("strictPluginOnlyCustomization", `["hooks"]`, true)
	ok("env", `{"MAX_THINKING_TOKENS":5}`, false) // nested property
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

// Concurrent writers of different keys must all land: the read-modify-write is locked and atomic.
func TestWriteRepoDefaultsConcurrentWritersKeepAllKeys(t *testing.T) {
	dir := vaulttest.Use(t)
	file := filepath.Join(dir, filepath.FromSlash(RepoDefaultsRel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{\n  \"$schema\": \"x\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys := []string{"prefersReducedMotion", "spinnerTipsEnabled", "awaySummaryEnabled", "agentPushNotifEnabled",
		"inputNeededNotifEnabled", "useAutoModeDuringPlan", "autoConnectIde", "disableClaudeAiConnectors",
		"skipWorkflowUsageWarning", "switchModelsOnFlag", "wheelScrollAccelerationEnabled"}
	var wg sync.WaitGroup
	errs := make(chan error, len(keys))
	for _, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := WriteRepoDefaults(map[string]json.RawMessage{k: json.RawMessage("false")})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(file)
	for _, k := range keys {
		if !strings.Contains(string(got), fmt.Sprintf("%q: false", k)) {
			t.Errorf("lost update: %s missing from\n%s", k, got)
		}
	}
	if ents, _ := os.ReadDir(filepath.Dir(file)); len(ents) != 2 { // settings.json + .lock
		t.Errorf("stray files beside the repo defaults: %v", ents)
	}
}

// autoMemoryDirectory merges every repo's memory into one flat dir, so Merge still drops it.
func TestMergeDropsLegacyAutoMemoryDirectory(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := Merge([]byte(`{"autoMemoryDirectory":"/flat","zzForeign":1}`), rendered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "autoMemoryDirectory") || !strings.Contains(string(out), "zzForeign") {
		t.Errorf("merge output:\n%s", out)
	}
}
