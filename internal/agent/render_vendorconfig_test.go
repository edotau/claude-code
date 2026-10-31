package agent_test

import (
	"bytes"
	"encoding/json"
	"github.com/edotau/claude-code/internal/agent"
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
	if _, err := agent.RenderConfigs(agent.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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
	if _, err := agent.RenderConfigs(agent.RenderOptions{Home: home, ForceHomes: true, Out: &out2, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("second agent.RenderConfigs: %v\n%s", err, out2.String())
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2) != content {
		t.Errorf("second run not byte-identical:\nfirst:\n%s\nsecond:\n%s", content, raw2)
	}
}

// topLevelKeyCount counts "key = " assignments in content that land before the first top-level
// [table] header (TOML has no way for such a key to belong to any table), skipping bracket-nested
// and multiline-string content the same way agent.InsertTopLevelTOML does.
func topLevelKeyCount(content, key string) int {
	count := 0
	for _, line := range strings.SplitAfter(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			break
		}
		if strings.HasPrefix(trimmed, key+" ") {
			count++
		}
	}
	return count
}

// The managed banner's approval_policy/sandbox_mode are the vendor spec's top-level keys; a
// pre-existing top-level key of the same name must be removed (not duplicated), while a same-named
// key inside an unrelated table ([profiles.x]) is left alone.
func TestCodexBannerReplacesExistingTopLevelPermissionKeys(t *testing.T) {
	home := scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "approval_policy = \"on-request\"\n\n[profiles.x]\napproval_policy = \"untrusted\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agent.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)

	if n := topLevelKeyCount(content, "approval_policy"); n != 1 {
		t.Fatalf("top-level approval_policy count = %d, want 1 (stale key must be removed, not duplicated):\n%s", n, content)
	}
	if !strings.Contains(content, "approval_policy = \"never\"") {
		t.Errorf("top-level approval_policy must be the banner's value (never):\n%s", content)
	}
	if !strings.Contains(content, "[profiles.x]") || !strings.Contains(content, "approval_policy = \"untrusted\"") {
		t.Errorf("profiles.x table's approval_policy must stay intact:\n%s", content)
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
	if _, err := agent.RenderConfigs(agent.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"copilot"}}); err != nil {
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
	if _, err := agent.RenderConfigs(agent.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"copilot"}}); err != nil {
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
			home := scratch(t)
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
				if _, err := agent.RenderConfigs(agent.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"copilot"}}); err != nil {
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
