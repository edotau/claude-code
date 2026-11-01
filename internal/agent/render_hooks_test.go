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

// orcaMarker is the literal foreign-hook command a third-party tool (Orca) installs into every
// lifecycle event; real bytes captured in testdata/codex-hooks-with-orca.json (= ~/.codex/hooks.json.backup).
const orcaMarker = "orca-agent-hook-form=1"

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRenderCodexHooksFilePreservesOrcaOnDisk is the end-to-end RED/GREEN: seed a scratch ~/.codex/hooks.json
// with the real pre-render bytes, run the full Codex render, and show Orca survives on disk.
func TestRenderCodexHooksFilePreservesOrcaOnDisk(t *testing.T) {
	home := scratch(t)
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, readTestdata(t, "codex-hooks-with-orca.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agent.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), orcaMarker); n != 9 {
		t.Fatalf("expected all 9 Orca groups to survive the render, found %d:\n%s", n, got)
	}
	if !strings.Contains(string(got), "claude-code hook PreToolUse --safety-scan") {
		t.Errorf("rendered codex hooks missing the current harness hook:\n%s", got)
	}
}

// TestMergeCopilotHooksKeepsForeignEntries mirrors the Codex case for Copilot's flat hook schema
// (no real captured bytes for this vendor; constructed from its own documented shape).
func TestMergeCopilotHooksKeepsForeignEntries(t *testing.T) {
	existing := []byte(`{
  "version": 1,
  "hooks": {
    "preToolUse": [
      {"type": "command", "matcher": "", "bash": ": orca-agent-hook-form=1; echo hi", "timeoutSec": 10},
      {"type": "command", "matcher": "bash", "bash": "$HOME/.claude/bin/claude-code hook safety-scan-STALE", "timeoutSec": 5}
    ],
    "sessionStart": [
      {"type": "command", "matcher": "", "bash": ": orca-agent-hook-form=1; echo start", "timeoutSec": 10}
    ]
  }
}`)
	body, err := agent.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := agent.MergeCopilotHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCopilotHooks: %v", err)
	}
	got := string(merged)
	if strings.Count(got, orcaMarker) != 2 {
		t.Fatalf("expected both foreign Orca entries to survive:\n%s", got)
	}
	if strings.Contains(got, "safety-scan-STALE") {
		t.Errorf("stale harness entry should be replaced, not kept:\n%s", got)
	}
	if !strings.Contains(got, "claude-code hook PreToolUse --safety-scan") {
		t.Errorf("merged output lost the current harness safety-scan hook:\n%s", got)
	}
}

// canonicalJSON sorts map keys (Go's own json.Marshal order for map[string]any) so two JSON values with
// different field order and whitespace can be compared for value-for-value equality.
func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("canonicalJSON: %v\n%s", err, raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// findCodexHook walks every PreToolUse group's hooks for the one whose "command" matches want.
func findCodexHook(t *testing.T, merged []byte, want string) map[string]any {
	t.Helper()
	var doc struct {
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("findCodexHook: %v\n%s", err, merged)
	}
	for _, group := range doc.Hooks["PreToolUse"] {
		for _, h := range group.Hooks {
			if h["command"] == want {
				return h
			}
		}
	}
	t.Fatalf("hook %q not found in merged output:\n%s", want, merged)
	return nil
}

// compactJSON strips whitespace only (json.Compact), unlike canonicalJSON it does NOT reorder keys through
// a map round-trip, so it is the right comparison for "preserved byte-for-byte modulo the document-wide
// pretty-printing pass", including key order.
func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compactJSON: %v\n%s", err, raw)
	}
	return buf.String()
}

// TestMergeCopilotHooksPreservesUnknownTopLevelKey mirrors the Codex case for Copilot's top-level shape.
func TestMergeCopilotHooksPreservesUnknownTopLevelKey(t *testing.T) {
	existing := []byte(`{"version":1,"hooks":{},"vendorExtra":"keep-me"}`)
	body, err := agent.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := agent.MergeCopilotHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCopilotHooks: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["vendorExtra"]) != `"keep-me"` {
		t.Fatalf("unknown top-level key dropped or altered: %s\n%s", doc["vendorExtra"], merged)
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
	_, err := agent.RenderConfigs(agent.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}})
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

// codexGroupOrder decodes merged into the ordered list of raw groups for one event, so a test can
// assert position (not just presence) the way json.RawMessage slices preserve it through marshal.
func codexGroupOrder(t *testing.T, merged []byte, event string) []json.RawMessage {
	t.Helper()
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("codexGroupOrder: %v\n%s", err, merged)
	}
	return doc.Hooks[event]
}

// TestMergeCopilotHooksIdempotent mirrors the Codex idempotence proof for Copilot's flat entry shape.
func TestMergeCopilotHooksIdempotent(t *testing.T) {
	existing := []byte(`{
  "version": 1,
  "hooks": {
    "preToolUse": [
      {"type": "command", "matcher": "", "bash": ": orca-agent-hook-form=1; echo hi", "timeoutSec": 10},
      {"type": "command", "matcher": "bash", "bash": "$HOME/.claude/bin/claude-code hook safety-scan-STALE", "timeoutSec": 5}
    ]
  }
}`)
	body, err := agent.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	once, err := agent.MergeCopilotHooks(existing, body)
	if err != nil {
		t.Fatalf("first agent.MergeCopilotHooks: %v", err)
	}
	twice, err := agent.MergeCopilotHooks(once, body)
	if err != nil {
		t.Fatalf("second agent.MergeCopilotHooks: %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("merge is not idempotent:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

// TestMergeCopilotHooksForeignFirstStaysFirst is the Copilot-side order-preserving RED fixture: its
// existing fixture already has Orca's entry before the stale harness one, so lock in that the merge
// keeps it there instead of pushing it after the rendered block.
func TestMergeCopilotHooksForeignFirstStaysFirst(t *testing.T) {
	existing := []byte(`{
  "version": 1,
  "hooks": {
    "preToolUse": [
      {"type": "command", "matcher": "", "bash": ": orca-agent-hook-form=1; echo hi", "timeoutSec": 10},
      {"type": "command", "matcher": "bash", "bash": "$HOME/.claude/bin/claude-code hook safety-scan-STALE", "timeoutSec": 5}
    ]
  }
}`)
	body, err := agent.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := agent.MergeCopilotHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCopilotHooks: %v", err)
	}
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	entries := doc.Hooks["preToolUse"]
	if len(entries) < 2 {
		t.Fatalf("expected Orca's entry plus the rendered one, got %d:\n%s", len(entries), merged)
	}
	if !strings.Contains(string(entries[0]), orcaMarker) {
		t.Fatalf("Orca's entry must stay first, got:\n%s\nfull:\n%s", entries[0], merged)
	}
	if strings.Contains(string(merged), "safety-scan-STALE") {
		t.Errorf("stale harness entry should have been replaced:\n%s", merged)
	}
}

// TestMergeCopilotHooksPreservesPowershellCwdEnv is the Copilot-side RED fixture: a foreign entry's
// powershell/cwd/env fields must survive, not be dropped by a decode into the narrower rendered shape.
func TestMergeCopilotHooksPreservesPowershellCwdEnv(t *testing.T) {
	existing := []byte(`{
  "version": 1,
  "hooks": {
    "preToolUse": [
      {"type": "command", "matcher": "", "bash": "orca-cmd", "powershell": "orca.ps1", "cwd": "/tmp/orca", "env": {"FOO": "bar"}, "timeoutSec": 10}
    ]
  }
}`)
	body, err := agent.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := agent.MergeCopilotHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCopilotHooks: %v", err)
	}
	var doc struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, e := range doc.Hooks["preToolUse"] {
		if e["bash"] == "orca-cmd" {
			found = e
		}
	}
	if found == nil {
		t.Fatalf("foreign entry missing from merged output:\n%s", merged)
	}
	if found["powershell"] != "orca.ps1" || found["cwd"] != "/tmp/orca" {
		t.Errorf("powershell/cwd dropped: %#v\n%s", found, merged)
	}
	env, _ := found["env"].(map[string]any)
	if env["FOO"] != "bar" {
		t.Errorf("env dropped: %#v\n%s", found, merged)
	}
}
