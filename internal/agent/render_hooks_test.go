package agent_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"
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
	home := scratch(t)
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, readTestdata(t, "codex-hooks-with-foreign.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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
	home := scratch(t)
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
	_, err := agent.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}})
	if err == nil {
		t.Fatalf("expected agent.RenderConfigs to fail on a malformed hooks.json, got no error:\n%s", out.String())
	}
	got, readErr := os.ReadFile(tomlPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != originalToml {
		t.Fatalf("config.toml was written despite the hooks merge failing:\nwant:\n%s\ngot:\n%s", originalToml, got)
	}
}
