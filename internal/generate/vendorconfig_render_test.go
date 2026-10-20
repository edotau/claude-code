package generate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Approval/sandbox top-level keys land before the first [table] header, never inside one, and a
// second run is byte-identical (idempotent), even with pre-existing foreign tables in the file.
func TestCodexApprovalKeysPrecedeExistingTables(t *testing.T) {
	home := scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "[projects.\"/x\"]\ntrust_level = \"trusted\"\n\n[hooks.state.\"k\"]\ntrusted_hash = \"sha256:deadbeef\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	firstTable := strings.Index(content, "[")
	if firstTable < 0 {
		t.Fatalf("no table header found at all:\n%s", content)
	}
	approvalIdx := strings.Index(content, "approval_policy")
	sandboxIdx := strings.Index(content, "sandbox_mode")
	if approvalIdx < 0 || sandboxIdx < 0 {
		t.Fatalf("approval_policy/sandbox_mode not rendered:\n%s", content)
	}
	// Before the first "[" means top-level: TOML has no way for a key there to belong to any table.
	if approvalIdx > firstTable || sandboxIdx > firstTable {
		t.Errorf("approval_policy/sandbox_mode (at %d, %d) must precede the first [table] header (at %d):\n%s",
			approvalIdx, sandboxIdx, firstTable, content)
	}
	if !strings.Contains(content, "[projects.\"/x\"]") || !strings.Contains(content, "trust_level = \"trusted\"") {
		t.Errorf("foreign [projects] table was dropped or altered:\n%s", content)
	}
	if !strings.Contains(content, "[hooks.state.\"k\"]") || !strings.Contains(content, "trusted_hash = \"sha256:deadbeef\"") {
		t.Errorf("foreign [hooks.state] table was dropped or altered:\n%s", content)
	}

	var out2 bytes.Buffer
	if _, err := Run(Options{Home: home, Out: &out2, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("second Run: %v\n%s", err, out2.String())
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2) != content {
		t.Errorf("second run not byte-identical:\nfirst:\n%s\nsecond:\n%s", content, raw2)
	}
}

// renderCopilotSettings merges defaultPermissionMode into ~/.copilot/settings.json, keeping every
// other key (foreign scalar and array alike) untouched.
func TestCopilotSettingsMergesDefaultPermissionModeKeepingOtherKeys(t *testing.T) {
	home := scratch(t)
	path := filepath.Join(home, ".copilot", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"theme":"default","allowedUrls":["https://example.com"]}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Out: &out, Agents: []string{"copilot"}}); err != nil {
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

// A hand-broken (not valid JSON) copilot settings.json is left byte-for-byte untouched rather than
// clobbered or "fixed" into a fresh document.
func TestCopilotSettingsLeavesInvalidJSONUntouched(t *testing.T) {
	home := scratch(t)
	path := filepath.Join(home, ".copilot", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "{not valid json"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Run(Options{Home: home, Out: &out, Agents: []string{"copilot"}}); err != nil {
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
