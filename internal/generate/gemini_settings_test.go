package generate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func geminiSettingsPath(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// renderGeminiSettings merges the spec's exact-match guard denies into agy's settings.json,
// keeping every other key untouched; a second run is byte-identical.
func TestGeminiSettingsMergesDenyGuardsKeepingOtherKeys(t *testing.T) {
	home := scratch(t)
	path := geminiSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{"model":"x","permissions":{"allow":["read_url(antigravity.google)"],"deny":["command(user rule)"]},"trustedWorkspaces":["/a"]}`
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings.json not valid JSON: %v\n%s", err, raw)
	}
	if got["model"] != "x" {
		t.Errorf("foreign key \"model\" dropped or altered: %s", raw)
	}
	tw, ok := got["trustedWorkspaces"].([]any)
	if !ok || len(tw) != 1 || tw[0] != "/a" {
		t.Errorf("foreign key \"trustedWorkspaces\" dropped or altered: %s", raw)
	}
	perms, ok := got["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions key missing or wrong type: %s", raw)
	}
	allow, ok := perms["allow"].([]any)
	if !ok || len(allow) != 1 || allow[0] != "read_url(antigravity.google)" {
		t.Errorf("permissions.allow dropped or altered: %s", raw)
	}
	deny, ok := perms["deny"].([]any)
	if !ok {
		t.Fatalf("permissions.deny missing or wrong type: %s", raw)
	}
	wantDeny := []any{"command(user rule)", "command(rm -rf /)", "command(rm -rf /*)", "command(rm -rf ~)"}
	if !reflect.DeepEqual(deny, wantDeny) {
		t.Errorf("permissions.deny = %v, want %v", deny, wantDeny)
	}

	var out2 bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out2, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("second Run: %v\n%s", err, out2.String())
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2) != string(raw) {
		t.Errorf("second run not byte-identical:\nfirst:\n%s\nsecond:\n%s", raw, raw2)
	}
}

// A prefix guard rule (Bash(x:*)) is not expressible as an agy exact-match deny and is skipped;
// ask-rules never render for agy, only deny does.
func TestGeminiSettingsSkipsPrefixAndAskRules(t *testing.T) {
	home := scratch(t)
	// vendors.json overlay lives at CLAUDE_CONFIG_DIR/env.d/vendors.json; scratch() already set
	// CLAUDE_CONFIG_DIR, so resolve the overlay path the same way vendorconfig does.
	overlayDir := mkEnvD(t, os.Getenv("CLAUDE_CONFIG_DIR"))
	overlay := `{"guardRules":{"deny":["Bash(git push --force:*)","Bash(rm -rf /)"],"ask":["Bash(git reset --hard:*)"]}}`
	if err := os.WriteFile(filepath.Join(overlayDir, "vendors.json"), []byte(overlay), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(geminiSettingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings.json not valid JSON: %v\n%s", err, raw)
	}
	perms, ok := got["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions key missing: %s", raw)
	}
	deny, _ := perms["deny"].([]any)
	want := []any{"command(rm -rf /)"}
	if !reflect.DeepEqual(deny, want) {
		t.Errorf("permissions.deny = %v, want %v (prefix and ask rules must not appear)", deny, want)
	}
}

// A hand-broken (not valid JSON) agy settings.json is left byte-for-byte untouched, never clobbered.
func TestGeminiSettingsLeavesInvalidJSONUntouched(t *testing.T) {
	home := scratch(t)
	path := geminiSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "{not valid json"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != broken {
		t.Errorf("invalid settings.json was modified: got %q, want %q", raw, broken)
	}
}

// No pre-existing agy settings.json: the file is created holding only the guard denies.
func TestGeminiSettingsMissingFileCreatesDenyOnly(t *testing.T) {
	home := scratch(t)
	path := geminiSettingsPath(home)
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("precondition: %s must not exist", path)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("settings.json was not created: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("created settings.json not valid JSON: %v\n%s", err, raw)
	}
	perms, ok := got["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions key missing: %s", raw)
	}
	deny, ok := perms["deny"].([]any)
	if !ok {
		t.Fatalf("permissions.deny missing: %s", raw)
	}
	wantDeny := []any{"command(rm -rf /)", "command(rm -rf /*)", "command(rm -rf ~)"}
	if !reflect.DeepEqual(deny, wantDeny) {
		t.Errorf("permissions.deny = %v, want %v", deny, wantDeny)
	}
}

// An agy settings.json whose top-level JSON value is `null` or an array (not an object) must be
// left byte-for-byte untouched, and must never panic (a nil map write, not a type-assertion skip).
func TestGeminiSettingsNullJSONUntouched(t *testing.T) {
	for _, content := range []string{"null", "[]"} {
		t.Run(content, func(t *testing.T) {
			home := scratch(t)
			path := geminiSettingsPath(home)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Run panicked on %s settings.json: %v", content, r)
					}
				}()
				var out bytes.Buffer
				if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
					t.Fatalf("Run: %v\n%s", err, out.String())
				}
			}()

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != content {
				t.Errorf("%s settings.json was modified: got %q, want %q", content, raw, content)
			}
		})
	}
}
