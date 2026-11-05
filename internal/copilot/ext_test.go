package copilot_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/agentconfig/render/rendertest"
)

// seedCopilotSettings writes content as home's ~/.copilot/settings.json and returns its path.
func seedCopilotSettings(t *testing.T, home, content string) string {
	t.Helper()
	path := filepath.Join(home, ".copilot", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// renderCopilotOnly renders just the copilot config into home.
func renderCopilotOnly(t *testing.T, home string) {
	t.Helper()
	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"copilot"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
}

// readSettingsJSON reads path and decodes it as a JSON object, failing the test otherwise.
func readSettingsJSON(t *testing.T, path string) (got map[string]any, raw []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings.json not valid JSON: %v\n%s", err, raw)
	}
	return got, raw
}

// renderCopilotSettings merges defaultPermissionMode into ~/.copilot/settings.json, keeping every
// other key (foreign scalar and array alike) untouched.
func TestCopilotSettingsMergesDefaultPermissionModeKeepingOtherKeys(t *testing.T) {
	home := rendertest.Scratch(t)
	path := seedCopilotSettings(t, home, `{"theme":"default","allowedUrls":["https://example.com"]}`)
	renderCopilotOnly(t, home)

	got, raw := readSettingsJSON(t, path)
	if got["defaultPermissionMode"] != "assisted" {
		t.Errorf("defaultPermissionMode = %v, want assisted: %s", got["defaultPermissionMode"], raw)
	}
	if got["theme"] != "default" {
		t.Errorf("foreign key \"theme\" dropped or altered: %s", raw)
	}
	urls, ok := got["allowedUrls"].([]any)
	if !ok || len(urls) != 1 || urls[0] != "https://example.com" {
		t.Errorf("foreign key \"allowedUrls\" dropped or altered: %s", raw)
	}
}

// A hand-broken (not valid JSON) copilot settings.json is left byte-for-byte untouched rather than
// clobbered or "fixed" into a fresh document.
func TestCopilotSettingsLeavesInvalidJSONUntouched(t *testing.T) {
	home := rendertest.Scratch(t)
	path := filepath.Join(home, ".copilot", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "{not valid json"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"copilot"}}); err != nil {
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

// A copilot settings.json whose top-level JSON value is `null` or an array (not an object) must be
// left byte-for-byte untouched, and must never panic (a nil map write, not a type-assertion skip).
func TestCopilotSettingsNullJSONUntouched(t *testing.T) {
	for _, content := range []string{"null", "[]"} {
		t.Run(content, func(t *testing.T) {
			home := rendertest.Scratch(t)
			path := filepath.Join(home, ".copilot", "settings.json")
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
				if _, err := agent.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"copilot"}}); err != nil {
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
