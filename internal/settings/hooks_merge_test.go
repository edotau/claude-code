package settings_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/edotau/claude-code/internal/agent/vendors"
	"github.com/edotau/claude-code/internal/settings"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/hookspec"
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
	merged, err := settings.MergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	got := string(merged)
	if !strings.Contains(got, "claude-code hook PreToolUse --safety-scan") {
		t.Errorf("merged output lost the harness safety-scan hook:\n%s", got)
	}
	var doc struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("merged output is not valid JSON: %v\n%s", err, got)
	}
	for _, event := range []string{
		"PostToolUse", "PreToolUse", "UserPromptSubmit", "SessionStart",
		"PermissionRequest", "SubagentStart", "SubagentStop", "Stop", "Interrupt",
	} {
		groups, _ := json.Marshal(doc.Hooks[event])
		if !strings.Contains(string(groups), orcaMarker) {
			t.Errorf("event %s lost Orca's entry: %s", event, groups)
		}
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
	merged, err := settings.MergeCodexHooks(existing, body)
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
	if _, err := settings.Run(settings.Options{Router: vendors.Router{}, Home: home, Vendors: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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
	body, err := hookspec.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCopilotHooks(existing, body)
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

// TestMergeCodexHooksPreservesUnknownFieldsAndNoTimeout is the RED fixture for the typed-struct-decode
// bug: a foreign Codex hook with an unknown key and no timeout must keep both, byte-for-value, not gain
// an injected "timeout": 0 nor lose "customKey".
func TestMergeCodexHooksPreservesUnknownFieldsAndNoTimeout(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"orca-stop","customKey":1}]}]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCodexHooks(existing, body)
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

// TestMergeCodexHooksRoundTripsForeignGroupByteIdentical proves a foreign group (one the filter never
// touches, because none of its hooks are harness-owned) survives the merge as the exact same bytes it
// started as, including its key order and an unknown "extra" group-level key - not just an equivalent value.
func TestMergeCodexHooksRoundTripsForeignGroupByteIdentical(t *testing.T) {
	original := json.RawMessage(`{"matcher":"Bash","extra":{"b":2,"a":1},"hooks":[{"type":"command","command":"orca-stop","async":true,"nested":{"b":2,"a":1}}]}`)
	existing := []byte(`{"hooks":{"PreToolUse":[` + string(original) + `]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCodexHooks(existing, body)
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
		if compactJSON(t, g) == compactJSON(t, original) {
			found = g
		}
	}
	if found == nil {
		t.Fatalf("foreign group did not round-trip byte-identical; want:\n%s\ngot:\n%s", original, merged)
	}
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

// TestMergeCodexHooksPreservesUnknownTopLevelKey is the RED fixture for the top-level decode: an existing
// hooks.json key outside "hooks" (e.g. a future Codex field) must survive the merge untouched.
func TestMergeCodexHooksPreservesUnknownTopLevelKey(t *testing.T) {
	existing := []byte(`{"hooks":{},"futureField":{"nested":true,"n":1}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["futureField"]) == "" || canonicalJSON(t, doc["futureField"]) != canonicalJSON(t, json.RawMessage(`{"nested":true,"n":1}`)) {
		t.Fatalf("unknown top-level key dropped or altered:\n%s", merged)
	}
}

// TestMergeCopilotHooksPreservesUnknownTopLevelKey mirrors the Codex case for Copilot's top-level shape.
func TestMergeCopilotHooksPreservesUnknownTopLevelKey(t *testing.T) {
	existing := []byte(`{"version":1,"hooks":{},"vendorExtra":"keep-me"}`)
	body, err := hookspec.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCopilotHooks(existing, body)
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

// TestMergeCodexHooksPreservesLessThanUnescaped is the RED fixture for HTML-escaping: a foreign hook
// command containing a literal less-than sign must survive as-is, not become the six-byte escape
// json.MarshalIndent emits by default.
func TestMergeCodexHooksPreservesLessThanUnescaped(t *testing.T) {
	lt := string(rune(0x3c))
	escaped := string([]byte{0x5c, 'u', '0', '0', '3', 'c'}) // the escape sequence, spelled out so the source never contains a literal backslash-u003c
	cmd := "test $a " + lt + " $b && echo orca-stop"
	existing := []byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"` + cmd + `"}]}]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	if !strings.Contains(string(merged), cmd) {
		t.Fatalf("literal less-than was HTML-escaped:\n%s", merged)
	}
	if strings.Contains(string(merged), escaped) {
		t.Fatalf("output contains an HTML-escaped less-than:\n%s", merged)
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
	_, err := settings.Run(settings.Options{Router: vendors.Router{}, Home: home, Vendors: true, Out: &out, Agents: []string{"codex"}})
	if err == nil {
		t.Fatalf("expected settings.Run to fail on a malformed hooks.json, got no error:\n%s", out.String())
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

// TestMergeCodexHooksForeignFirstStaysFirst is the order-preserving RED fixture: when Orca's group
// precedes the harness group in the existing file, it must still precede the rendered block after the
// merge, not get pushed after it (the old prepend-always behavior).
func TestMergeCodexHooksForeignFirstStaysFirst(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"hooks":[{"type":"command","command":"orca-stop"}]},
		{"matcher":"Bash","hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook safety-scan-STALE"}]}
	]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	groups := codexGroupOrder(t, merged, "PreToolUse")
	if len(groups) < 2 {
		t.Fatalf("expected at least 2 groups (orca + rendered), got %d:\n%s", len(groups), merged)
	}
	if !strings.Contains(string(groups[0]), "orca-stop") {
		t.Fatalf("Orca's group must stay first, got first group:\n%s\nfull:\n%s", groups[0], merged)
	}
	if strings.Contains(string(merged), "safety-scan-STALE") {
		t.Errorf("stale harness command should have been replaced:\n%s", merged)
	}
	if !strings.Contains(string(groups[1]), "claude-code hook PreToolUse --safety-scan") {
		t.Errorf("rendered block should follow Orca's group, got second group:\n%s\nfull:\n%s", groups[1], merged)
	}
}

// TestMergeCodexHooksHarnessFirstStaysFirst mirrors the above the other way: the harness group already
// precedes Orca's, so the rendered block replaces it in place and Orca's group stays last.
func TestMergeCodexHooksHarnessFirstStaysFirst(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"matcher":"Bash","hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook safety-scan-STALE"}]},
		{"hooks":[{"type":"command","command":"orca-stop"}]}
	]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	groups := codexGroupOrder(t, merged, "PreToolUse")
	if len(groups) < 2 {
		t.Fatalf("expected at least 2 groups (rendered + orca), got %d:\n%s", len(groups), merged)
	}
	last := groups[len(groups)-1]
	if !strings.Contains(string(last), "orca-stop") {
		t.Fatalf("Orca's group must stay last, got last group:\n%s\nfull:\n%s", last, merged)
	}
	if !strings.Contains(string(groups[0]), "claude-code hook PreToolUse --safety-scan") {
		t.Errorf("rendered block should take the harness group's old position (first), got:\n%s\nfull:\n%s", groups[0], merged)
	}
}

// TestMergeCodexHooksNoHarnessItemPrepends is the no-existing-harness-item RED fixture: an event with
// only foreign groups gets the rendered block prepended (the old, and still correct, behavior for this case).
func TestMergeCodexHooksNoHarnessItemPrepends(t *testing.T) {
	existing := []byte(`{"hooks":{"PostToolUse":[
		{"hooks":[{"type":"command","command":"orca-start"}]}
	]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	groups := codexGroupOrder(t, merged, "PostToolUse")
	if len(groups) < 2 {
		t.Fatalf("expected rendered block + Orca's group, got %d:\n%s", len(groups), merged)
	}
	if strings.Contains(string(groups[0]), "orca-start") {
		t.Fatalf("rendered block must be prepended ahead of Orca when there was no harness item, got first group:\n%s\nfull:\n%s", groups[0], merged)
	}
	if !strings.Contains(string(groups[len(groups)-1]), "orca-start") {
		t.Fatalf("Orca's group should be last, got:\n%s\nfull:\n%s", groups[len(groups)-1], merged)
	}
}

// TestMergeCodexHooksMixedGroupSplicesInPlace is the mixed-group RED fixture: a single existing group
// holding both a foreign and a harness-owned hook still marks the rendered block's splice position, and
// its rebuilt (foreign-only) remainder is emitted right after that block, keeping any further foreign
// group after it in original order.
func TestMergeCodexHooksMixedGroupSplicesInPlace(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"hooks":[{"type":"command","command":"orca-A"}]},
		{"hooks":[
			{"type":"command","command":"orca-mixed"},
			{"type":"command","command":"$HOME/.claude/bin/claude-code hook safety-scan-STALE"}
		]},
		{"hooks":[{"type":"command","command":"orca-C"}]}
	]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("mergeCodexHooks: %v", err)
	}
	groups := codexGroupOrder(t, merged, "PreToolUse")
	indexOf := func(marker string) int {
		for i, g := range groups {
			if strings.Contains(string(g), marker) {
				return i
			}
		}
		t.Fatalf("marker %q not found in merged PreToolUse:\n%s", marker, merged)
		return -1
	}
	idxA, idxMixed, idxC := indexOf("orca-A"), indexOf("orca-mixed"), indexOf("orca-C")
	idxRendered := indexOf("claude-code hook PreToolUse --safety-scan")
	if idxA >= idxRendered || idxRendered >= idxMixed || idxMixed >= idxC {
		t.Fatalf("expected order orca-A < rendered < rebuilt-mixed < orca-C, got A=%d rendered=%d mixed=%d C=%d:\n%s",
			idxA, idxRendered, idxMixed, idxC, merged)
	}
	if strings.Contains(string(merged), "safety-scan-STALE") {
		t.Errorf("stale harness hook inside the mixed group should have been dropped, not kept:\n%s", merged)
	}
}

// TestMergeCodexHooksIdempotent proves merging the already-merged file again over the same rendered
// body reproduces it byte-for-byte: the second pass sees only harness-owned (just-rendered) groups plus
// the untouched foreign ones, so nothing should move or change.
func TestMergeCodexHooksIdempotent(t *testing.T) {
	existing := readTestdata(t, "codex-hooks-with-orca.json")
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	once, err := settings.MergeCodexHooks(existing, body)
	if err != nil {
		t.Fatalf("first settings.MergeCodexHooks: %v", err)
	}
	twice, err := settings.MergeCodexHooks(once, body)
	if err != nil {
		t.Fatalf("second settings.MergeCodexHooks: %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("merge is not idempotent:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
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
	body, err := hookspec.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	once, err := settings.MergeCopilotHooks(existing, body)
	if err != nil {
		t.Fatalf("first settings.MergeCopilotHooks: %v", err)
	}
	twice, err := settings.MergeCopilotHooks(once, body)
	if err != nil {
		t.Fatalf("second settings.MergeCopilotHooks: %v", err)
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
	body, err := hookspec.CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCopilotHooks(existing, body)
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

// TestCodexTrustEntriesSurviveForeignFirstOrder pins trusted_hash keys to the merged file's positions, harness hooks only.
func TestCodexTrustEntriesSurviveForeignFirstOrder(t *testing.T) {
	home := scratch(t)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Orca's group precedes the harness one in PreToolUse: the fixture this whole change exists for.
	hooksPath := filepath.Join(codexDir, "hooks.json")
	seed := []byte(`{"hooks":{"PreToolUse":[
		{"hooks":[{"type":"command","command":"orca-stop"}]},
		{"matcher":"Bash","hooks":[{"type":"command","command":"$HOME/.claude/bin/claude-code hook safety-scan-STALE"}]}
	]}}`)
	if err := os.WriteFile(hooksPath, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := settings.Run(settings.Options{Router: vendors.Router{}, Home: home, Vendors: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := settings.CodexTrustEntries(hooksPath, body)
	if err != nil {
		t.Fatal(err)
	}
	toml, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	src := settings.CodexHookSourcePath(hooksPath)
	// Rendered alone, safety-scan sits at 0:0; behind Orca's group it is 1:0, and Orca's slot gets no entry.
	safetyScan := rendered[src+":pre_tool_use:0:0"]
	want := fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = %q", src+":pre_tool_use:1:0", safetyScan)
	if safetyScan == "" || !strings.Contains(string(toml), want) {
		t.Errorf("config.toml lacks %s:\n%s", want, toml)
	}
	if strings.Contains(string(toml), src+":pre_tool_use:0:0") {
		t.Errorf("config.toml trusts Orca's slot pre_tool_use:0:0:\n%s", toml)
	}
	merged, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), "orca-stop") {
		t.Errorf("Orca's hook dropped from hooks.json:\n%s", merged)
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
	merged, err := settings.MergeCopilotHooks(existing, body)
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

// A stale vendor hooks file naming retired hooks is re-rendered without them (what install does on drift).
func TestMergeCodexHooksDropsStaleHarnessNames(t *testing.T) {
	stale := []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"HARNESS_HOOK_AGENT=codex $HOME/.claude/bin/claude-code hook bash-guard"}]}],` +
		`"PostToolUse":[{"matcher":"Read","hooks":[{"type":"command","command":"HARNESS_HOOK_AGENT=codex $HOME/.claude/bin/claude-code hook access-trace"}]}]}}`)
	body, err := hookspec.CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := settings.MergeCodexHooks(stale, body)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"bash-guard", "access-trace"} {
		if strings.Contains(string(merged), old) {
			t.Errorf("stale %s survived the re-render", old)
		}
	}
}
