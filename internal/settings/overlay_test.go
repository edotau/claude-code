package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/defaults"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestApplyOverlaySwapsKnownKey: applyOverlay must replace a rendered value with the overlay's value for a
// key the catalog recognizes, leaving every other key untouched.
func TestApplyOverlaySwapsKnownKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	ov := map[string]json.RawMessage{"theme": json.RawMessage(`"dark"`)}
	out, err := applyOverlay(rendered, ov)
	if err != nil {
		t.Fatal(err)
	}
	var obj, base map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rendered, &base); err != nil {
		t.Fatal(err)
	}
	if string(obj["theme"]) != `"dark"` {
		t.Errorf("theme = %s, want \"dark\"", obj["theme"])
	}
	for k, v := range base { // every other key is left exactly as rendered, whatever the defaults hold
		got, _ := compactStr(obj[k])
		want, _ := compactStr(v)
		if k != "theme" && got != want {
			t.Errorf("unrelated key %s changed: %s → %s", k, v, obj[k])
		}
	}
}

// TestApplyOverlayCarriesAutoModeObject: autoMode is overlay-only (Section==""); applyOverlay must still
// place it into the rendered output with the object value intact, even when a string inside it contains
// a path-like token such as "<repo>".
func TestApplyOverlayCarriesAutoModeObject(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	want := json.RawMessage(`{"soft_deny":["$defaults"],"environment":"/Users/eric/<repo>/worktrees"}`)
	ov := map[string]json.RawMessage{"autoMode": want}
	out, err := applyOverlay(rendered, ov)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	var gotVal, wantVal any
	if err := json.Unmarshal(obj["autoMode"], &gotVal); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Errorf("autoMode = %s, want %s", obj["autoMode"], want)
	}
}

// TestApplyOverlayRejectsUnknownKey: an overlay entry for a key the catalog does not recognize must error —
// a silently-accepted typo would never reach settings.json and the user would never know why.
func TestApplyOverlayRejectsUnknownKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	ov := map[string]json.RawMessage{"notARealSetting": json.RawMessage(`"x"`)}
	if _, err := applyOverlay(rendered, ov); err == nil {
		t.Error("applyOverlay with an unknown key = nil error, want rejection")
	}
}

// TestOverlayRoundTrip: SaveOverlay then LoadOverlay, in an isolated CLAUDE_CONFIG_DIR, must return the same
// entries, and RenderLive must apply them on top of the template render.
func TestOverlayRoundTrip(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	empty, err := LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("LoadOverlay on a fresh config dir = %v, want empty", empty)
	}

	ov := map[string]json.RawMessage{"theme": json.RawMessage(`"light"`)}
	if err := SaveOverlay(ov); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if string(got["theme"]) != `"light"` {
		t.Fatalf("LoadOverlay after SaveOverlay: theme = %s, want \"light\"", got["theme"])
	}

	live, err := RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(live, &obj); err != nil {
		t.Fatal(err)
	}
	if string(obj["theme"]) != `"light"` {
		t.Errorf("RenderLive theme = %s, want \"light\" from the overlay", obj["theme"])
	}
}

// TestOverlayRejectsInvalidValue: SaveOverlay must reject a value that fails the catalog entry's Validate —
// an overlay is only useful if it can never render settings.json into an invalid state.
func TestOverlayRejectsInvalidValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ov := map[string]json.RawMessage{"cleanupPeriodDays": json.RawMessage(`0`)}
	if err := SaveOverlay(ov); err == nil {
		t.Error("SaveOverlay with cleanupPeriodDays=0 = nil error, want rejection")
	}
}

// TestOverlayAcceptsAnySchemaKey: a schema key outside the Catalog applies at top level, a schema env variable
// lands inside env, and a key the schema lacks is still refused.
func TestOverlayAcceptsAnySchemaKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	out, err := applyOverlay(rendered, map[string]json.RawMessage{
		"wheelScrollAccelerationEnabled": json.RawMessage("false"),
		"DISABLE_TELEMETRY":              json.RawMessage(`"1"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _ := parseObject(out)
	env, _ := parseObject(o.vals["env"])
	if string(o.vals["wheelScrollAccelerationEnabled"]) != "false" || string(env.vals["DISABLE_TELEMETRY"]) != `"1"` {
		t.Errorf("overlay keys not applied:\n%s", out)
	}
	if _, ok := o.vals["DISABLE_TELEMETRY"]; ok {
		t.Error("env variable leaked to the top level")
	}
	if _, err := applyOverlay(rendered, map[string]json.RawMessage{"noSuchKey": json.RawMessage("1")}); err == nil {
		t.Error("unknown key accepted")
	}
}

// TestWriteRepoDefaultsKeepsSchemaFirst: staged keys land in the file (env variables under env), $schema stays
// first even from a file that had it misplaced, and a staged key outside the schema is refused.
func TestWriteRepoDefaultsKeepsSchemaFirst(t *testing.T) {
	dir := vaulttest.Use(t)
	file := filepath.Join(dir, filepath.FromSlash(RepoDefaultsRel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	o, err := parseObject(defaults.Claude)
	if err != nil {
		t.Fatal(err)
	}
	o.keys = append(o.keys[1:], o.keys[0]) // $schema last
	seed, _ := o.marshal()
	if err := os.WriteFile(file, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	staged := map[string]json.RawMessage{
		"wheelScrollAccelerationEnabled": json.RawMessage("false"),
		"DISABLE_TELEMETRY":              json.RawMessage(`"1"`),
	}
	if _, err := WriteRepoDefaults(map[string]json.RawMessage{"noSuchKey": json.RawMessage("1")}); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := WriteRepoDefaults(staged); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	back, err := parseObject(got)
	if err != nil {
		t.Fatal(err)
	}
	if back.keys[0] != "$schema" {
		t.Errorf("first key = %q, want $schema", back.keys[0])
	}
	if string(back.vals["wheelScrollAccelerationEnabled"]) != "false" || !strings.Contains(string(back.vals["env"]), `"DISABLE_TELEMETRY": "1"`) {
		t.Errorf("staged keys missing:\n%s", got)
	}
}

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
}

func TestCredentialsRefusedByWriters(t *testing.T) {
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
		{"ANTHROPIC_CUSTOM_HEADERS": json.RawMessage(`"x-api-key: sk-ant-abcdefghijklmnop"`)},
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
		wg.Go(func() {
			_, err := WriteRepoDefaults(map[string]json.RawMessage{k: json.RawMessage("false")})
			errs <- err
		})
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

func TestStructuralKeysRefusedAndObjectConstraintsEnforced(t *testing.T) {
	dir := vaulttest.Use(t)
	file := filepath.Join(dir, filepath.FromSlash(RepoDefaultsRel))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{\n  \"$schema\": \"x\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"hooks", "statusLine", "$schema", "permissions", "env"} {
		bad := map[string]json.RawMessage{k: json.RawMessage(`{}`)}
		if err := SaveOverlay(bad); err == nil || !strings.Contains(err.Error(), "structural") {
			t.Errorf("SaveOverlay(%s) = %v, want a structural refusal", k, err)
		}
		if _, err := WriteRepoDefaults(bad); err == nil {
			t.Errorf("WriteRepoDefaults(%s) accepted", k)
		}
	}
	if err := SaveOverlay(map[string]json.RawMessage{"MAX_THINKING_TOKENS": json.RawMessage(`"5"`)}); err != nil {
		t.Errorf("an env variable must stay editable: %v", err)
	}
}

func TestSchemaReferencesRemainStructural(t *testing.T) {
	for _, k := range SchemaKeys() {
		if k.HasRef() && !StructuralKey(k.Name) {
			t.Errorf("%s uses $ref but is still editable", k.Name)
		}
	}
	if pk, _ := SchemaKey("permissions"); !pk.HasRef() {
		t.Error("permissions should be flagged as using $ref")
	}
}

func TestObjectConstraintsEnforced(t *testing.T) {
	k, _ := SchemaKey("subagentStatusLine")
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

// UpdateOverlay folds a change into the stored overlay, so two writers' keys both survive.
func TestUpdateOverlayKeepsConcurrentKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SaveOverlay(map[string]json.RawMessage{"theme": json.RawMessage(`"light"`)}); err != nil {
		t.Fatal(err)
	}
	err := UpdateOverlay(func(cur map[string]json.RawMessage) map[string]json.RawMessage {
		cur["verbose"] = json.RawMessage(`true`)
		return cur
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := LoadOverlay()
	if string(got["theme"]) != `"light"` || string(got["verbose"]) != `true` {
		t.Errorf("overlay after update = %v, want theme and verbose", got)
	}
}

// TestSaveOverlayReplacesCorruptEntry: SaveOverlay writes whole, so an unparseable stored overlay never blocks a repair.
func TestSaveOverlayReplacesCorruptEntry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, overlayBucketKey, "{not json") }); err != nil {
		t.Fatal(err)
	}
	if err := SaveOverlay(map[string]json.RawMessage{"theme": json.RawMessage(`"dark"`)}); err != nil {
		t.Fatalf("SaveOverlay over a corrupt entry: %v", err)
	}
	if got, err := LoadOverlay(); err != nil || string(got["theme"]) != `"dark"` {
		t.Errorf("overlay = %v, %v; want theme dark", got, err)
	}
}

// Concurrent UpdateOverlay writers each read inside the vault transaction, so every key survives.
func TestUpdateOverlayConcurrentWritersAllSurvive(t *testing.T) {
	vaulttest.Use(t)
	keys := []string{"verbose", "prefersReducedMotion", "inputNeededNotifEnabled", "disableClaudeAiConnectors",
		"useAutoModeDuringPlan", "skipWorkflowUsageWarning"}
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Go(func() {
			err := UpdateOverlay(func(cur map[string]json.RawMessage) map[string]json.RawMessage {
				cur[k] = json.RawMessage(`true`)
				return cur
			})
			if err != nil {
				t.Errorf("UpdateOverlay %s: %v", k, err)
			}
		})
	}
	wg.Wait()
	got, err := LoadOverlay()
	if err != nil || len(got) != len(keys) {
		t.Errorf("overlay = %v (%v), want %d keys", got, err, len(keys))
	}
}
