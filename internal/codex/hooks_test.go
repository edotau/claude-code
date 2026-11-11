package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/client"
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

// The full Codex render keeps every foreign hook group in an existing hooks.json (real pre-render bytes).
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
	if _, err := client.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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
	_, err := client.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}})
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
	home := scratch(t)
	originalToml := []byte("# existing config must survive a failed hooks write\n")
	tomlPath := seedCodexFile(t, home, "config.toml", originalToml)
	hooksPath := seedCodexFile(t, home, "hooks.json", []byte(`{"hooks":{}}`))
	var out bytes.Buffer
	plan, drift, err := client.PrepareConfigs(agentconfig.RenderOptions{
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
	if !strings.Contains(string(raw), `"Write|Edit|apply_patch"`) { // docs-placement and docs-plan-gate
		t.Errorf("no file-write gate selects Codex's apply_patch:\n%s", raw)
	}
}

// A symlinked hooks.json is written through to its target; the link survives.
func TestWriteCodexHooksFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "real.json"), filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexHooks(link, []byte("{}"), []byte(`{"a":1}`), true); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("hooks.json symlink was replaced: %v, %v", info, err)
	}
	if got, _ := os.ReadFile(target); string(got) != `{"a":1}` {
		t.Errorf("target = %q, want the new hooks", got)
	}
}
