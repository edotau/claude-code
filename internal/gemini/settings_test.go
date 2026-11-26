package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An interactive gateway launch already carries the pin's --effort: the record effort replaces it, never doubles it.
func TestAgyInteractiveEffortReplacesPinSuffix(t *testing.T) {
	s := agent.Settings{Agent: "gemini", Effort: "low"}
	argv := []string{"--model", "gemini-3.8-flash", "--effort", "high"}
	_, got, _, _, err := geminiEngine{agent.NewBaseEngine("gemini")}.RenderSettings(context.Background(), s, nil, argv)
	if want := []string{"--model", "gemini-3.8-flash", "--effort", "low"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("argv = %q (err %v), want %q", got, err, want)
	}
	if argv[3] != "high" {
		t.Errorf("caller's argv mutated: %q", argv)
	}
}

// A stored useGuardRules "false" stops the install render merging guard denies into agy's settings.json.
func TestRenderGeminiSettingsFollowsGuardExtra(t *testing.T) {
	for _, c := range []struct {
		name string
		rec  *vault.AgentRecord
		want bool
	}{{"built-in true", nil, true}, {"record false", &vault.AgentRecord{Extras: map[string]string{extraUseGuardRules: "false"}}, false},
		{"record unset keeps it on", &vault.AgentRecord{Extras: map[string]string{extraUseGuardRules: ""}}, true}} {
		t.Run(c.name, func(t *testing.T) {
			vaulttest.Use(t)
			home := t.TempDir()
			if c.rec != nil {
				if err := vault.SetAgentRecord("gemini", *c.rec); err != nil {
					t.Fatal(err)
				}
			}
			got, err := renderGeminiSettings(home, agentconfig.RenderOptions{Home: home, DryRun: true}, io.Discard)
			if err != nil || got != c.want {
				t.Errorf("settings.json would change = %v (err %v), want %v", got, err, c.want)
			}
		})
	}
}

func geminiSettingsPath(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// templateDeny is the settings template's bare-command deny list as rendered for gemini (17ff29d5).
var templateDeny = []any{"command(rm -rf /)", "command(rm -rf ~)"}

// renderGeminiSettings merges the guard denies into agy's settings.json, keeping other keys; a rerun is byte-identical.
func TestGeminiSettingsMergesDenyGuardsKeepingOtherKeys(t *testing.T) {
	home := agenttest.Scratch(t)
	if err := os.WriteFile(settings.SettingsFile(), []byte(`{"permissions":{"deny":["Bash(owner-command)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := geminiSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{"model":"x","permissions":{"allow":["read_url(antigravity.google)"],"deny":["command(user rule)"]},"trustedWorkspaces":["/a"]}`
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	checkMergedSettings(t, raw)

	var out2 bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out2, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("second agent.RenderConfigs: %v\n%s", err, out2.String())
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2) != string(raw) {
		t.Errorf("second run not byte-identical:\nfirst:\n%s\nsecond:\n%s", raw, raw2)
	}
}

// checkMergedSettings requires foreign keys intact and the user's deny rule followed by the live denies.
func checkMergedSettings(t *testing.T, raw []byte) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings.json not valid JSON: %v\n%s", err, raw)
	}
	if got["model"] != "x" {
		t.Errorf("foreign key \"model\" dropped or altered: %s", raw)
	}
	tw, ok := got["trustedWorkspaces"].([]any)
	if !ok || len(tw) != 1 || tw[0] != "/a" {
		t.Errorf("foreign key \"trustedWorkspaces\" dropped or altered: %s", raw)
	}
	checkMergedPermissions(t, got, raw)
}

// checkMergedPermissions requires allow untouched and deny as the user's rule plus the live denies.
func checkMergedPermissions(t *testing.T, got map[string]any, raw []byte) {
	t.Helper()
	perms, ok := got["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions key missing or wrong type: %s", raw)
	}
	allow, ok := perms["allow"].([]any)
	if !ok || len(allow) != 1 || allow[0] != "read_url(antigravity.google)" {
		t.Errorf("permissions.allow dropped or altered: %s", raw)
	}
	deny, ok := perms["deny"].([]any)
	if !ok {
		t.Fatalf("permissions.deny missing or wrong type: %s", raw)
	}
	wantDeny := []any{"command(user rule)", "command(owner-command)"}
	if !reflect.DeepEqual(deny, wantDeny) {
		t.Errorf("permissions.deny = %v, want %v", deny, wantDeny)
	}
}

// A prefix guard rule (Bash(x:*)) is not expressible as an agy exact-match deny and is skipped.
func TestGeminiSettingsSkipsPrefixRules(t *testing.T) {
	home := agenttest.Scratch(t)
	live := `{"permissions":{"deny":["Bash(git push --force:*)","Bash(rm -rf /)"]}}`
	if err := os.WriteFile(settings.SettingsFile(), []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(geminiSettingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings.json not valid JSON: %v\n%s", err, raw)
	}
	perms, ok := got["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions key missing: %s", raw)
	}
	deny, _ := perms["deny"].([]any)
	want := []any{"command(rm -rf /)"}
	if !reflect.DeepEqual(deny, want) {
		t.Errorf("permissions.deny = %v, want %v (a prefix rule must not appear)", deny, want)
	}
}

// A hand-broken (not valid JSON) agy settings.json is left byte-for-byte untouched, never clobbered.
func TestGeminiSettingsLeavesInvalidJSONUntouched(t *testing.T) {
	home := agenttest.Scratch(t)
	path := geminiSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "{not valid json"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "gemini-settings: skipped: ") {
		t.Errorf("output lacks the skip note:\n%s", out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != broken {
		t.Errorf("invalid settings.json was modified: got %q, want %q", raw, broken)
	}
}

// No pre-existing agy settings.json: the file is created holding only the guard denies.
func TestGeminiSettingsMissingFileCreatesDenyOnly(t *testing.T) {
	home := agenttest.Scratch(t)
	path := geminiSettingsPath(home)
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("precondition: %s must not exist", path)
	}

	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("settings.json was not created: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("created settings.json not valid JSON: %v\n%s", err, raw)
	}
	perms, ok := got["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions key missing: %s", raw)
	}
	deny, ok := perms["deny"].([]any)
	if !ok {
		t.Fatalf("permissions.deny missing: %s", raw)
	}
	wantDeny := templateDeny
	if !reflect.DeepEqual(deny, wantDeny) {
		t.Errorf("permissions.deny = %v, want %v", deny, wantDeny)
	}
}

// An agy settings.json that is `null` or an array is left byte-for-byte untouched and must not panic.
func TestGeminiSettingsNullJSONUntouched(t *testing.T) {
	for _, content := range []string{"null", "[]"} {
		t.Run(content, func(t *testing.T) {
			home := agenttest.Scratch(t)
			path := geminiSettingsPath(home)
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
				if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
					t.Fatalf("Run: %v\n%s", err, out.String())
				}
				if !strings.Contains(out.String(), "gemini-settings: skipped: ") {
					t.Errorf("output lacks the skip note:\n%s", out.String())
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

// An unreadable agy settings.json (here a directory) is skipped, never mistaken for a missing file and overwritten.
func TestGeminiSettingsUnreadableIsSkipped(t *testing.T) {
	home := agenttest.Scratch(t)
	path := geminiSettingsPath(home)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "gemini-settings: skipped: cannot read") {
		t.Errorf("output lacks the cannot-read skip note:\n%s", out.String())
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatalf("path was replaced: %v", err)
	}
}

// The agy hook render keeps foreign hooks and replaces its own entry with one gate hook on agy's tool names.
func TestRenderGeminiHooks(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".gemini", "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mine":{"Stop":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{`"mine"`, `HARNESS_HOOK_AGENT=gemini $HOME/.claude/bin/.hookd PreToolUse --safety-scan`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("hooks.json lacks %s:\n%s", want, b)
		}
	}
}

// agyHookEntry is a group (Matcher, Hooks) or a flat handler (Command, Timeout) in agy's hooks.json.
type agyHookEntry struct {
	Matcher string
	Hooks   []struct{ Command string }
	Command string
	Timeout int
}

// agy loads SessionStart and Stop as flat handler lists and tool events as matcher groups, in agy's tool names.
func TestRenderGeminiHooksShapePerEvent(t *testing.T) {
	home := t.TempDir()
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".gemini", "config", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string][]agyHookEntry
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	h := doc["claude-code"]
	for _, e := range h["SessionStart"] {
		if e.Command == "" || e.Matcher != "" || e.Hooks != nil || e.Timeout == 0 {
			t.Errorf("SessionStart entry must be a flat handler: %+v", e)
		}
	}
	for _, e := range h["Stop"] {
		if e.Command == "" || e.Hooks != nil {
			t.Errorf("Stop entry must be a flat handler: %+v", e)
		}
	}
	if len(h["SessionStart"]) != 1 || len(h["Stop"]) != 3 {
		t.Errorf("SessionStart/Stop handlers = %d/%d, want 1/3", len(h["SessionStart"]), len(h["Stop"]))
	}
	for _, event := range []string{"PreToolUse", "PostToolUse"} {
		for _, g := range h[event] {
			if g.Command != "" || len(g.Hooks) != 1 || strings.ContainsAny(g.Matcher, "BWE") {
				t.Errorf("%s entry must be a group on agy tool names: %+v", event, g)
			}
		}
	}
	for _, want := range []string{"run_command", "write_file|edit_file"} {
		if !slices.ContainsFunc(h["PostToolUse"], func(g agyHookEntry) bool { return g.Matcher == want }) {
			t.Errorf("PostToolUse lacks a group on %q", want)
		}
	}
}

// A hooks.json that does not parse is an error naming the file, never a silent skip, and stays as it was.
func TestRenderGeminiHooksMalformedIsAnError(t *testing.T) {
	for _, bad := range []string{"{broken", "[1]", "null", `{"a":1} x`, `{"a":1}}`} {
		home := t.TempDir()
		path := agyHooksPath(home)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := stagedHooks(home)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%q: err = %v, want one naming %s", bad, err, path)
		}
		if b, _ := os.ReadFile(path); string(b) != bad {
			t.Errorf("%q: file rewritten to %q", bad, b)
		}
	}
}

// Foreign entries keep their exact bytes and order; an unchanged harness entry means no rewrite at all.
func TestRenderGeminiHooksKeepsForeignBytes(t *testing.T) {
	home := t.TempDir()
	path := agyHooksPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := `"foreign" : { "PreToolUse" : [ { "matcher" : "Bash", "hooks" : [] } ] }`
	stale := `"claude-code" : {"PreToolUse":[]}`
	for _, seed := range []string{"{" + foreign + "}", "{" + stale + ", " + foreign + "}", "{" + foreign + ", " + stale + "}"} {
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		if changed, err := stagedHooks(home); err != nil || !changed {
			t.Fatalf("seed %q: changed = %v, err = %v; want a rewrite", seed, changed, err)
		}
		b, _ := os.ReadFile(path)
		if !strings.Contains(string(b), foreign) {
			t.Errorf("seed %q: foreign entry changed:\n%s", seed, b)
		}
		if (strings.Index(string(b), `"claude-code"`) < strings.Index(string(b), `"foreign"`)) != strings.HasPrefix(seed, "{"+stale) {
			t.Errorf("seed %q: entry order changed:\n%s", seed, b)
		}
		if changed, err := stagedHooks(home); err != nil || changed {
			t.Errorf("seed %q: second render changed = %v, err = %v; want none", seed, changed, err)
		}
		if again, _ := os.ReadFile(path); !bytes.Equal(again, b) {
			t.Errorf("seed %q: a no-change render rewrote the file", seed)
		}
	}
}

// An existing harness entry equal in value but spaced differently is not rewritten.
func TestRenderGeminiHooksSpacingIsNotDrift(t *testing.T) {
	home := t.TempDir()
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	path := agyHooksPath(home)
	b, _ := os.ReadFile(path)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	respaced := append(append([]byte("{ \"claude-code\" :  "), bytes.ReplaceAll(obj["claude-code"], []byte("\n"), nil)...), " }"...)
	if err := os.WriteFile(path, respaced, 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := stagedHooks(home); err != nil || changed {
		t.Errorf("respaced entry: changed = %v, err = %v; want none", changed, err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, respaced) {
		t.Errorf("respaced file rewritten:\n%s", got)
	}
}

func stagedHooks(home string) (bool, error) {
	var plan agentconfig.RenderPlan
	changed, err := renderGeminiHooks(home, agentconfig.RenderOptions{}.Stage(&plan), io.Discard)
	if err != nil {
		return changed, err
	}
	return changed, plan.Apply()
}
