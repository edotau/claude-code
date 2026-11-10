package codex_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/client"
	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/agentconfig/render/rendertest"
)

// foreignMarker is the literal foreign-hook command a third-party tool (e.g. an agent dashboard) installs into every
// lifecycle event; real bytes captured in testdata/codex-hooks-with-foreign.json (= ~/.codex/hooks.json.backup).
const foreignMarker = "foreign-agent-hook-form=1"

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRenderCodexHooksFilePreservesForeignOnDisk is the end-to-end RED/GREEN: seed a scratch ~/.codex/hooks.json
// with the real pre-render bytes, run the full Codex render, and show the foreign hooks survive on disk.
func TestRenderCodexHooksFilePreservesForeignOnDisk(t *testing.T) {
	home := rendertest.Scratch(t)
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, readTestdata(t, "codex-hooks-with-foreign.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), foreignMarker); n != 9 {
		t.Fatalf("expected all 9 foreign groups to survive the render, found %d:\n%s", n, got)
	}
	if !strings.Contains(string(got), "claude-code hook PreToolUse --safety-scan") {
		t.Errorf("rendered codex hooks missing the current harness hook:\n%s", got)
	}
}

// TestRenderCodexFailsHooksMergeLeavesConfigTomlUntouched is the RED fixture for partial-apply: a
// malformed existing ~/.codex/hooks.json must fail the whole render before config.toml (new trust hashes)
// is ever written, not after.
func TestRenderCodexFailsHooksMergeLeavesConfigTomlUntouched(t *testing.T) {
	home := rendertest.Scratch(t)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const originalToml = "# pre-existing config, untouched\n"
	tomlPath := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(originalToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}})
	if err == nil {
		t.Fatalf("expected client.RenderConfigs to fail on a malformed hooks.json, got no error:\n%s", out.String())
	}
	got, readErr := os.ReadFile(tomlPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != originalToml {
		t.Fatalf("config.toml was written despite the hooks merge failing:\nwant:\n%s\ngot:\n%s", originalToml, got)
	}
}

// seedCodexFile writes body to home/.codex/name and returns its path.
func seedCodexFile(t *testing.T, home, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(home, ".codex", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A hooks.json changed between prepare and apply fails the render before config.toml gets new trust hashes.
func TestRenderCodexHooksWriteConflictLeavesConfigTomlUntouched(t *testing.T) {
	home := rendertest.Scratch(t)
	originalToml := []byte("# existing config must survive a failed hooks write\n")
	tomlPath := seedCodexFile(t, home, "config.toml", originalToml)
	hooksPath := seedCodexFile(t, home, "hooks.json", []byte(`{"hooks":{}}`))
	var out bytes.Buffer
	plan, drift, err := client.PrepareConfigs(agentrender.RenderOptions{
		Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"},
	})
	if err != nil || !drift {
		t.Fatalf("prepare: drift=%v, err=%v\n%s", drift, err, out.String())
	}
	newerHooks := []byte(`{"hooks":{},"foreign":"concurrent edit"}`)
	seedCodexFile(t, home, "hooks.json", newerHooks)
	if err := plan.Apply(); err == nil || !strings.HasPrefix(err.Error(), "codex-hooks:") ||
		!strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("apply: expected hooks concurrency error, got %v\n%s", err, out.String())
	}
	for path, want := range map[string][]byte{tomlPath: originalToml, hooksPath: newerHooks} {
		if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
			t.Errorf("%s changed after the failed hooks write:\nwant:\n%s\ngot:\n%s", filepath.Base(path), want, got)
		}
	}
}

// Approval/sandbox top-level keys land before the first [table] header, never inside one, and a
// second run is byte-identical (idempotent), even with pre-existing foreign tables in the file.
func TestCodexApprovalKeysPrecedeExistingTables(t *testing.T) {
	home := rendertest.Scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "[projects.\"/x\"]\ntrust_level = \"trusted\"\n\n[hooks.state.\"k\"]\ntrusted_hash = \"sha256:deadbeef\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	assertCodexApprovalBeforeForeignTables(t, content)

	var out2 bytes.Buffer
	if _, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out2, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("second client.RenderConfigs: %v\n%s", err, out2.String())
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2) != content {
		t.Errorf("second run not byte-identical:\nfirst:\n%s\nsecond:\n%s", content, raw2)
	}
}

func assertCodexApprovalBeforeForeignTables(t *testing.T, content string) {
	t.Helper()
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
}

// topLevelKeyCount counts "key = " assignments in content that land before the first top-level
// [table] header (TOML has no way for such a key to belong to any table), skipping bracket-nested
// and multiline-string content the same way agentconfig.InsertTopLevelTOML does.
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
	home := rendertest.Scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "approval_policy = \"on-request\"\n\n[profiles.x]\napproval_policy = \"untrusted\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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
