package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Glob-free path denies and exact allows map to agy rules; Ask rides as allow; unmapped Allow/Ask are reported.
func TestAgyPermissionSettings(t *testing.T) {
	p := agent.Permissions{
		Deny:  []agent.Rule{{Tool: "Write", Arg: "denied2.txt"}, {Tool: "Read", Arg: "/abs/secret"}},
		Allow: []agent.Rule{{Tool: "Bash", Arg: "go test ./..."}, {Tool: "Bash", Arg: "git log:*"}},
		Ask:   []agent.Rule{{Tool: "WebFetch"}},
	}
	got, unmapped, err := agyPermissionSettings(p, true, true, "/w", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(got)
	var doc struct {
		Permissions struct{ Allow, Deny []string }
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("settings %q: %v", got, err)
	}
	for _, want := range []string{"write_file(/w/denied2.txt)", "read_file(/abs/secret)"} {
		if !slices.Contains(doc.Permissions.Deny, want) {
			t.Errorf("deny %v lacks %q", doc.Permissions.Deny, want)
		}
	}
	if !slices.Equal(doc.Permissions.Allow, []string{"command(go test ./...)"}) {
		t.Errorf("allow = %v", doc.Permissions.Allow)
	}
	if want := []string{"Bash(git log:*)", "WebFetch"}; !slices.Equal(unmapped, want) {
		t.Errorf("unmapped = %v, want %v", unmapped, want)
	}
	if s, _, _ := agyPermissionSettings(agent.Permissions{}, true, true, "/w", io.Discard); s != "" {
		t.Errorf("zero policy renders %q, want no --settings", s)
	}
}

// A Deny agy can't hold (a Bash deny with no gate hook installed, a glob path, a bare tool) or a failed guard load refuses the dispatch.
func TestAgyPermissionSettingsFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, r := range []agent.Rule{{Tool: "Bash", Arg: "touch x"}, {Tool: "Bash", Arg: "rm:*"}, {Tool: "Edit", Arg: "internal/**"}, {Tool: "WebFetch"}} {
		_, _, err := agyPermissionSettings(agent.Permissions{Deny: []agent.Rule{r}}, true, true, "/w", io.Discard)
		if err == nil || !strings.Contains(err.Error(), r.String()) {
			t.Errorf("deny %s: err = %v, want a refusal naming it", r, err)
		}
	}
	orig := loadAgentConfig
	loadAgentConfig = func() (*agentconfig.Config, error) { return nil, errors.New("overlay unreadable") }
	t.Cleanup(func() { loadAgentConfig = orig })
	_, _, err := agyPermissionSettings(agent.Permissions{Mode: "auto"}, true, true, "/w", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "overlay unreadable") {
		t.Errorf("failed guard load: err = %v, want it returned", err)
	}
}

// Plan mode with tools on is refused as fatal (agy cannot deny unmatched calls); without tools nothing can run.
func TestAgyPermissionSettingsRefusesPlan(t *testing.T) {
	p := agent.Permissions{Mode: "plan"}
	_, _, err := agyPermissionSettings(p, true, true, "/w", io.Discard)
	var ae *retry.Error
	if !errors.As(err, &ae) || ae.Kind != retry.KindFatal || !strings.Contains(err.Error(), "plan") {
		t.Errorf("plan with tools: err = %v, want a fatal refusal naming plan", err)
	}
	if _, _, err := agyPermissionSettings(p, true, false, "/w", io.Discard); err != nil {
		t.Errorf("plan without tools: %v", err)
	}
}

// With the gate hook installed a Bash Deny agy cannot express natively is the hook's: accepted, exact ones also ride native.
func TestAgyBashDenyNeedsGateHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.Permissions{Deny: []agent.Rule{{Tool: "Bash", Arg: "rm *"}, {Tool: "Bash", Arg: "touch x"}}}
	if _, _, err := agyPermissionSettings(p, true, true, "/w", io.Discard); err == nil || !strings.Contains(err.Error(), "Bash(rm *)") {
		t.Fatalf("no gate installed: err = %v, want a refusal naming Bash(rm *)", err)
	}
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	got, _, err := agyPermissionSettings(p, true, true, "/w", io.Discard)
	if err != nil || !strings.Contains(got, "command(touch x)") || strings.Contains(got, "rm *") {
		t.Fatalf("gate installed: settings %q err %v, want the exact deny only", got, err)
	}
}

func TestAgyBashDenyRejectsNonBlockingGate(t *testing.T) {
	command := (settings.EventSpec{Event: settings.EventPreToolUse, Name: "permission-gate"}).Command()
	for _, c := range []struct {
		name, event, matcher, command string
		async, workspace, wantGate    bool
	}{
		{"echo", "PreToolUse", "run_command", "echo --permission-gate", false, false, false},
		{"post-tool", "PostToolUse", "run_command", command, false, false, false},
		{"async", "PreToolUse", "run_command", command, true, false, false},
		{"wrong-tool", "PreToolUse", "read_file", command, false, false, false},
		{"partial-matcher", "PreToolUse", "run", command, false, false, false},
		{"invalid-matcher", "PreToolUse", "[", command, false, false, false},
		{"global", "PreToolUse", "run_command", command, false, false, true},
		{"workspace", "PreToolUse", "run_command", command, false, true, true},
		{"all-tools", "PreToolUse", "", command, false, false, true},
		{"agent-env", "PreToolUse", "run_command|read_file", settings.AgentEnv + "=gemini " + command, false, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			cwd := t.TempDir()
			path := agyHooksPath(home)
			if c.workspace {
				path = filepath.Join(cwd, ".agents", "hooks.json")
			}
			group := settings.MatcherGroup{Matcher: c.matcher, Hooks: []settings.Hook{{Type: "command", Command: c.command, Async: c.async}}}
			b, err := json.Marshal(map[string]any{"harness": map[string]any{c.event: []settings.MatcherGroup{group}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			p := agent.Permissions{Deny: []agent.Rule{{Tool: "Bash", Arg: "touch x"}}}
			_, _, err = agyPermissionSettings(p, true, true, cwd, io.Discard)
			if (err == nil) != c.wantGate {
				t.Fatalf("Bash deny with %s hook: err = %v, want gate accepted = %v", c.name, err, c.wantGate)
			}
		})
	}
}

// --read-only on agy is the bare Bash/Write/Edit denies: refused without the gate hook, the hook's alone once it is installed.
func TestAgyReadOnlyDenyNeedsGateHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.Permissions{Deny: agent.ReadOnlyDeny}
	_, _, err := agyPermissionSettings(p, true, true, "/w", io.Discard)
	for _, r := range []string{"Bash", "Write", "Edit"} { // literal: the read-only set itself is under test
		if err == nil || !strings.Contains(err.Error(), r) {
			t.Errorf("no gate installed: err = %v, want a refusal naming %s", err, r)
		}
	}
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	got, _, err := agyPermissionSettings(p, true, true, "/w", io.Discard)
	if err != nil || strings.Contains(got, "()") || strings.Contains(got, "write_file") {
		t.Fatalf("gate installed: settings %q err %v, want accepted with no native rule for a bare tool", got, err)
	}
}

// An empty cwd must not resolve the workspace hooks.json against the process directory.
func TestAgyGateCoversIgnoresWorkspaceFileForEmptyCwd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	command := (settings.EventSpec{Event: settings.EventPreToolUse, Name: "permission-gate"}).Command()
	group := settings.MatcherGroup{Hooks: []settings.Hook{{Type: "command", Command: command}}}
	b, err := json.Marshal(map[string]any{"harness": map[string]any{"PreToolUse": []settings.MatcherGroup{group}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agents", "hooks.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if agyGateCovers("", "run_command") {
		t.Fatal("empty cwd read ./.agents/hooks.json from the process directory")
	}
	if !agyGateCovers(dir, "run_command") {
		t.Fatal("control: the workspace file with the gate was not honored")
	}
}

// Only useGuardRules "false" drops the guard denies from --settings; any other value keeps them.
func TestAgyGuardRulesFromExtras(t *testing.T) {
	deny := agent.Permissions{Deny: []agent.Rule{{Tool: "Write", Arg: "x.txt"}}}
	for guard, want := range map[string]bool{"true": true, "false": false, "": true} {
		s := agent.Settings{Agent: "gemini", Headless: true, Tools: true, WorkDir: "/w", Permissions: deny,
			Record: vault.AgentRecord{Extras: map[string]string{extraUseGuardRules: guard}}}
		_, argv, _, cleanup, err := geminiEngine{agent.NewBaseEngine("gemini")}.RenderSettings(context.Background(), s, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		i := slices.Index(argv, "--settings")
		if i < 0 {
			t.Fatalf("useGuardRules=%q: argv %q lacks --settings", guard, argv)
		}
		if got := strings.Contains(argv[i+1], "command(rm -rf /)"); got != want {
			t.Errorf("useGuardRules=%q: guard denies present = %v, want %v (%s)", guard, got, want, argv[i+1])
		}
	}
}

// An empty record (the unreadable-vault fallback) on a headless run still carries the guard denies: fail closed.
func TestAgyGuardRulesEmptyRecordFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := stagedHooks(home); err != nil { // the gate hook, so the Bash deny is accepted
		t.Fatal(err)
	}
	s := agent.Settings{Agent: "gemini", Headless: true, Tools: true, WorkDir: "/w",
		Permissions: agent.Permissions{Deny: []agent.Rule{{Tool: "Write", Arg: "x.txt"}, {Tool: "Bash", Arg: "touch x"}}}}
	_, argv, _, _, err := geminiEngine{agent.NewBaseEngine("gemini")}.RenderSettings(context.Background(), s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(argv, "--settings")
	if i < 0 || !strings.Contains(argv[i+1], "command(rm -rf /)") || !strings.Contains(argv[i+1], "command(touch x)") {
		t.Fatalf("argv %q: the empty record dropped the guard denies", argv)
	}
}

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
var templateDeny = []any{"command(rm -rf /)", "command(rm -rf /*)", "command(rm -rf ~)"}

// renderGeminiSettings merges the spec's exact-match guard denies into agy's settings.json,
// keeping every other key untouched; a second run is byte-identical.
func TestGeminiSettingsMergesDenyGuardsKeepingOtherKeys(t *testing.T) {
	home := scratch(t)
	path := geminiSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{"model":"x","permissions":{"allow":["read_url(antigravity.google)"],"deny":["command(user rule)"]},"trustedWorkspaces":["/a"]}`
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	checkMergedSettings(t, raw)

	var out2 bytes.Buffer
	if _, err := client.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out2, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("second client.RenderConfigs: %v\n%s", err, out2.String())
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2) != string(raw) {
		t.Errorf("second run not byte-identical:\nfirst:\n%s\nsecond:\n%s", raw, raw2)
	}
}

// checkMergedSettings requires foreign keys intact and the user's deny rule followed by the template denies.
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

// checkMergedPermissions requires allow untouched and deny as the user's rule plus the template denies.
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
	wantDeny := append([]any{"command(user rule)"}, templateDeny...)
	if !reflect.DeepEqual(deny, wantDeny) {
		t.Errorf("permissions.deny = %v, want %v", deny, wantDeny)
	}
}

// A prefix guard rule (Bash(x:*)) is not expressible as an agy exact-match deny and is skipped;
// ask-rules never render for agy, only deny does.
func TestGeminiSettingsSkipsPrefixAndAskRules(t *testing.T) {
	home := scratch(t)
	overlay := `{"guardRules":{"deny":["Bash(git push --force:*)","Bash(rm -rf /)"],"ask":["Bash(git reset --hard:*)"]}}`
	vaulttest.Put(t, vault.Overlay, "agents", overlay)

	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
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
		t.Errorf("permissions.deny = %v, want %v (prefix and ask rules must not appear)", deny, want)
	}
}

// A hand-broken (not valid JSON) agy settings.json is left byte-for-byte untouched, never clobbered.
func TestGeminiSettingsLeavesInvalidJSONUntouched(t *testing.T) {
	home := scratch(t)
	path := geminiSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "{not valid json"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
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
	home := scratch(t)
	path := geminiSettingsPath(home)
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("precondition: %s must not exist", path)
	}

	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
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

// An agy settings.json whose top-level JSON value is `null` or an array (not an object) must be
// left byte-for-byte untouched, and must never panic (a nil map write, not a type-assertion skip).
func TestGeminiSettingsNullJSONUntouched(t *testing.T) {
	for _, content := range []string{"null", "[]"} {
		t.Run(content, func(t *testing.T) {
			home := scratch(t)
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
				if _, err := client.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
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
	home := scratch(t)
	path := geminiSettingsPath(home)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"gemini"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "gemini-settings: skipped: cannot read") {
		t.Errorf("output lacks the cannot-read skip note:\n%s", out.String())
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatalf("path was replaced: %v", err)
	}
}

// scratch sets a hermetic CLAUDE_CONFIG_DIR with a keyed default provider and returns the vendor-config home dir.
func scratch(t *testing.T) string {
	t.Helper()
	configDir := vaulttest.Use(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "")
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Cleanup(agent.StubRouter("http://127.0.0.1:9999"))
	vaulttest.Put(t, vault.Overlay, "providers", `{"default": "openai"}`)
	if err := os.MkdirAll(filepath.Join(configDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
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
	for _, want := range []string{`"mine"`, `HARNESS_HOOK_AGENT=gemini $HOME/.claude/bin/claude-code hook PreToolUse --permission-gate`, `"matcher": "run_command|read_file|write_file|edit_file"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("hooks.json lacks %s:\n%s", want, b)
		}
	}
}

// stagedHooks renders the agy hooks.json under home and applies the staged write.
func stagedHooks(home string) (bool, error) {
	var plan agentconfig.RenderPlan
	changed, err := renderGeminiHooks(home, agentconfig.RenderOptions{}.Stage(&plan), io.Discard)
	if err != nil {
		return changed, err
	}
	return changed, plan.Apply()
}
