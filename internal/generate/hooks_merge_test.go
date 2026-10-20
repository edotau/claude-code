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

// TestMergeCodexHooksPreservesUnknownFieldsAndNoTimeout is the RED fixture for the typed-struct-decode
// bug: a foreign Codex hook with an unknown key and no timeout must keep both, byte-for-value, not gain
// an injected "timeout": 0 nor lose "customKey".
func TestMergeCodexHooksPreservesUnknownFieldsAndNoTimeout(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"orca-stop","customKey":1}]}]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	hook := findCodexHook(t, merged, "orca-stop")
	if hook["customKey"] != float64(1) {
		t.Errorf("customKey dropped or altered: %#v\n%s", hook, merged)
	}
	if _, hasTimeout := hook["timeout"]; hasTimeout {
		t.Errorf("foreign hook with no timeout gained one: %#v\n%s", hook, merged)
	}
}

// TestMergeCodexHooksRoundTripsForeignGroupByteIdentical proves a foreign group's matcher and hook survive
// the merge value-for-value (canonicalized key order), not just that some marker string is present.
func TestMergeCodexHooksRoundTripsForeignGroupByteIdentical(t *testing.T) {
	original := json.RawMessage(`{"matcher":"Bash","hooks":[{"type":"command","command":"orca-stop","async":true,"nested":{"b":2,"a":1}}]}`)
	existing := []byte(`{"hooks":{"PreToolUse":[` + string(original) + `]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	var found json.RawMessage
	for _, g := range doc.Hooks["PreToolUse"] {
		if canonicalJSON(t, g) == canonicalJSON(t, original) {
			found = g
		}
	}
	if found == nil {
		t.Fatalf("foreign group did not round-trip value-for-value; want:\n%s\ngot:\n%s", canonicalJSON(t, original), merged)
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
	body, err := hookspec.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeCopilotHooks(existing, body)
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
