package generate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hookspec"
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

// TestMergeCodexHooksKeepsForeignEntries drives the merge with the real pre-render bytes
// (testdata/codex-hooks-with-orca.json = ~/.codex/hooks.json.backup) across every event Orca occupies.
func TestMergeCodexHooksKeepsForeignEntries(t *testing.T) {
	existing := readTestdata(t, "codex-hooks-with-orca.json")
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	got := string(merged)
	for _, event := range []string{
		"PostToolUse", "PreToolUse", "UserPromptSubmit", "SessionStart",
		"PermissionRequest", "SubagentStart", "SubagentStop", "Stop", "Interrupt",
	} {
		if n := strings.Count(got, orcaMarker); n < 9 {
			t.Fatalf("expected Orca's marker to survive in every one of its 9 groups, found %d occurrences in merged output for %s:\n%s", n, event, got)
		}
	}
	if !strings.Contains(got, "claude-code hook bash-guard") {
		t.Errorf("merged output lost the harness bash-guard hook:\n%s", got)
	}
	var doc struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("merged output is not valid JSON: %v\n%s", err, got)
	}
}

// TestMergeCodexHooksAgainstBrokenRender is the RED fixture: testdata/codex-hooks-broken-render.json is
// the real bytes a pre-fix render produced from codex-hooks-with-orca.json (no Orca entries at all).
// The merge of the same existing file must NOT reproduce that shape.
func TestMergeCodexHooksAgainstBrokenRender(t *testing.T) {
	existing := readTestdata(t, "codex-hooks-with-orca.json")
	broken := readTestdata(t, "codex-hooks-broken-render.json")
	if strings.Contains(string(broken), orcaMarker) {
		t.Fatal("fixture precondition: the broken render must not itself contain Orca's marker")
	}
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	if !strings.Contains(string(merged), orcaMarker) {
		t.Fatalf("merge reproduced the broken (Orca-dropping) render:\n%s", merged)
	}
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
	if _, err := Run(Options{Home: home, Vendors: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), orcaMarker); n != 9 {
		t.Fatalf("expected all 9 Orca groups to survive the render, found %d:\n%s", n, got)
	}
	if !strings.Contains(string(got), "claude-code hook bash-guard") {
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
      {"type": "command", "matcher": "bash", "bash": "$HOME/.claude/bin/claude-code hook bash-guard-STALE", "timeoutSec": 5}
    ],
    "sessionStart": [
      {"type": "command", "matcher": "", "bash": ": orca-agent-hook-form=1; echo start", "timeoutSec": 10}
    ]
  }
}`)
	body, err := hookspec.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeCopilotHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCopilotHooks: %v", err)
	}
	got := string(merged)
	if strings.Count(got, orcaMarker) != 2 {
		t.Fatalf("expected both foreign Orca entries to survive:\n%s", got)
	}
	if strings.Contains(got, "bash-guard-STALE") {
		t.Errorf("stale harness entry should be replaced, not kept:\n%s", got)
	}
	if !strings.Contains(got, "claude-code hook bash-guard") {
		t.Errorf("merged output lost the current harness bash-guard hook:\n%s", got)
	}
}
