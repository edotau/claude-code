package copilot_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/agent/client"
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
	if _, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"copilot"}}); err != nil {
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
	if got["defaultPermissionMode"] != "allow-all" {
		t.Errorf("defaultPermissionMode = %v, want allow-all: %s", got["defaultPermissionMode"], raw)
	}
	if got["theme"] != "default" {
		t.Errorf("foreign key \"theme\" dropped or altered: %s", raw)
	}
	urls, ok := got["allowedUrls"].([]any)
	if !ok || len(urls) != 1 || urls[0] != "https://example.com" {
		t.Errorf("foreign key \"allowedUrls\" dropped or altered: %s", raw)
	}
}

// A copilot settings.json that is not a JSON object (broken, `null`, an array) is left byte-for-byte untouched.
