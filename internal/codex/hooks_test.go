package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/settings"
)

// insertCodexSection puts the banner before the first real table, never at a "["-led line in a string or array.
func TestInsertCodexSectionSkipsBracketLinesInValues(t *testing.T) {
	for name, value := range map[string]string{
		"double-quoted string": "\"\"\"\nsome text\n[looks like a table]\n\"\"\"",
		"single-quoted string": "'''\nsome text\n[looks like a table]\n'''",
		"nested array":         "[\n  [1, 2],\n  [3, 4],\n]",
	} {
		in := "note = " + value + "\n\n[real.table]\nkey = 1\n"
		got := insertCodexSection(in, "[inserted]\nx = 1\n")
		at, inserted, table := strings.Index(got, "note = "+value+"\n"), strings.Index(got, "[inserted]"), strings.Index(got, "[real.table]")
		if at < 0 || inserted < 0 || at >= inserted || inserted >= table {
			t.Errorf("%s: want the value unchanged, then [inserted], then [real.table]; got:\n%s", name, got)
		}
	}
}

// foreignMarker is a third-party tool's hook command, captured in testdata/codex-hooks-with-foreign.json.
const foreignMarker = "foreign-agent-hook-form=1"

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// The full Codex render keeps every foreign hook group in an existing hooks.json (real pre-render bytes).
func TestRenderCodexHooksFilePreservesForeignOnDisk(t *testing.T) {
	home := agenttest.Scratch(t)
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, readTestdata(t, "codex-hooks-with-foreign.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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

// A malformed existing hooks.json fails the whole render before config.toml (new trust hashes) is written.
func TestRenderCodexFailsHooksMergeLeavesConfigTomlUntouched(t *testing.T) {
	home := agenttest.Scratch(t)
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
	_, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}})
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
	home := agenttest.Scratch(t)
	originalToml := []byte("# existing config must survive a failed hooks write\n")
	tomlPath := seedCodexFile(t, home, "config.toml", originalToml)
	hooksPath := seedCodexFile(t, home, "hooks.json", []byte(`{"hooks":{}}`))
	var out bytes.Buffer
	plan, drift, err := agent.PrepareConfigs(agentconfig.RenderOptions{
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

// Codex's hook file adds apply_patch exactly where a spec selects Write or Edit; every other matcher is the spec's own.
func TestCodexJSONAddsApplyPatchToFileWriteMatchers(t *testing.T) {
	raw, err := CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	rendered := map[string]string{}
	for _, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				rendered[h.Command] = g.Matcher
			}
		}
	}
	for _, s := range settings.SpecsFor("codex", "native") {
		want := s.Matcher
		if slices.ContainsFunc(strings.Split(s.Matcher, "|"), func(a string) bool { return a == "Write" || a == "Edit" }) {
			want += "|apply_patch"
		}
		if got, ok := rendered[settings.AgentCommand("codex", s)]; !ok || got != want {
			t.Errorf("%s: rendered matcher = %q (present %v), want %q", s.Name, got, ok, want)
		}
	}
	for _, s := range settings.SpecsFor("codex", "native") {
		if s.Name != "docs-placement" {
			continue
		}
		if m := rendered[settings.AgentCommand("codex", s)]; !strings.Contains(m, "apply_patch") || !strings.Contains(m, "Bash") {
			t.Errorf("docs-placement matcher = %q; want it to select apply_patch and Bash", m)
		}
		return
	}
	t.Error("docs-placement is not a Codex spec")
}

// The claim hooks and memory-start reach Codex: apply_patch edits are claimed, and each rendered hook has a trust hash.
func TestCodexJSONRendersClaimAndMemoryStartWithTrust(t *testing.T) {
	raw, err := CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	find := func(event, module string) (settings.MatcherGroup, bool) {
		for _, g := range doc.Hooks[event] {
			for _, h := range g.Hooks {
				if strings.HasSuffix(h.Command, "hook "+event+" --"+module) {
					return g, true
				}
			}
		}
		return settings.MatcherGroup{}, false
	}
	for event, module := range map[string]string{settings.EventPostToolUse: "worktree-claim", settings.EventPreToolUse: "worktree-claim-check"} {
		g, ok := find(event, module)
		if !ok || !slices.Contains(strings.Split(g.Matcher, "|"), "apply_patch") || !slices.Contains(strings.Split(g.Matcher, "|"), "Bash") {
			t.Errorf("%s on %s: group %+v (present %v), want a matcher selecting apply_patch and Bash", module, event, g, ok)
		}
	}
	if g, ok := find(settings.EventSessionStart, "memory-start"); !ok || g.Matcher != "" {
		t.Errorf("memory-start group %+v (present %v), want the unmatched SessionStart group", g, ok)
	}
	trust, err := codexTrustEntries(filepath.Join(t.TempDir(), "hooks.json"), raw)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(settings.SpecsFor("codex", "native")); len(trust) != want {
		t.Errorf("%d trust entries for %d rendered hooks", len(trust), want)
	}
	for key := range trust {
		if !strings.Contains(key, ":post_tool_use:") {
			continue
		}
		return
	}
	t.Error("no post_tool_use trust entry: the claim hook would be untrusted")
}
