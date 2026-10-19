package generate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeOpencodePluginsOverlay appends "opencodePlugins" to the scratch() providers.json overlay
// (configDir is CLAUDE_CONFIG_DIR, already pointed there by scratch).
func writeOpencodePluginsOverlay(t *testing.T, plugins []string) {
	t.Helper()
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	overlay := map[string]any{"default": "openai"}
	if plugins != nil {
		overlay["opencodePlugins"] = plugins
	}
	body, err := json.Marshal(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "providers.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Non-empty reg.OpencodePlugins renders the "plugin" key and claims it in x-claude-code-managed.
func TestRenderOpenCodePluginNonEmptyWritesAndManages(t *testing.T) {
	home := scratch(t)
	writeOpencodePluginsOverlay(t, []string{"opencode-gemini-auth@1.4.15"})

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Out: &out, Agents: []string{"opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oc map[string]any
	if err := json.Unmarshal(raw, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v\n%s", err, raw)
	}
	plugin, _ := oc["plugin"].([]any)
	if len(plugin) != 1 || plugin[0] != "opencode-gemini-auth@1.4.15" {
		t.Errorf("plugin = %v, want [\"opencode-gemini-auth@1.4.15\"]: %s", oc["plugin"], raw)
	}
	managed, _ := oc["x-claude-code-managed"].([]any)
	found := false
	for _, v := range managed {
		if v == "plugin" {
			found = true
		}
	}
	if !found {
		t.Errorf("x-claude-code-managed = %v, want \"plugin\" claimed", managed)
	}
}

// Empty reg.OpencodePlugins leaves a hand-added "plugin" key untouched and never claims it as managed.
func TestRenderOpenCodePluginEmptyLeavesUserKeyAlone(t *testing.T) {
	home := scratch(t)
	writeOpencodePluginsOverlay(t, nil) // no opencodePlugins key at all: the registry default (empty)

	ocPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(ocPath), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"plugin": ["my-own-plugin@1.0.0"]}`
	if err := os.WriteFile(ocPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Out: &out, Agents: []string{"opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatal(err)
	}
	var oc map[string]any
	if err := json.Unmarshal(raw, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v\n%s", err, raw)
	}
	plugin, _ := oc["plugin"].([]any)
	if len(plugin) != 1 || plugin[0] != "my-own-plugin@1.0.0" {
		t.Errorf("foreign \"plugin\" key was touched: %s", raw)
	}
	managed, _ := oc["x-claude-code-managed"].([]any)
	for _, v := range managed {
		if v == "plugin" {
			t.Errorf("x-claude-code-managed claims \"plugin\", want it left unclaimed: %v", managed)
		}
	}
}
