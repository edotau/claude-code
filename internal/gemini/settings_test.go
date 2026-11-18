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
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Glob-free path denies and exact allows map to agy rules
func TestAgyPermissionSettings(t *testing.T) {
	p := agent.Permissions{
		Deny:  []agent.Rule{{Tool: "Write", Arg: "denied2.txt"}, {Tool: "Read", Arg: "/abs/secret"}},
		Allow: []agent.Rule{{Tool: "Bash", Arg: "go test ./..."}, {Tool: "Bash", Arg: "git log:*"}},
		Ask:   []agent.Rule{{Tool: "WebFetch"}},
	}
	got, notes, err := agyPermissionSettings(p, true, true, "/w")
	if err != nil {
		t.Fatal(err)
	}
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
	if want := []string{"ask WebFetch runs as allow (agy has no headless judge)", "Bash(git log:*)", "WebFetch"}; !slices.Equal(notes, want) {
		t.Errorf("notes = %q, want %q", notes, want)
	}
	if s, _, _ := agyPermissionSettings(agent.Permissions{}, true, true, "/w"); s != "" {
		t.Errorf("zero policy renders %q, want no --settings", s)
	}
}

// Write and Edit each map to both native write tools; a path is absolute as given or joined to a non-empty cwd.
func TestAgyRulesWriteToolsAndPaths(t *testing.T) {
	both := func(p string) []string { return []string{"write_file(" + p + ")", "edit_file(" + p + ")"} }
	for _, c := range []struct {
		name string
		rule agent.Rule
		cwd  string
		want []string
	}{
		{"write relative", agent.Rule{Tool: "Write", Arg: "a.txt"}, "/w", both("/w/a.txt")},
		{"edit relative", agent.Rule{Tool: "Edit", Arg: "a.txt"}, "/w", both("/w/a.txt")},
		{"absolute ignores cwd", agent.Rule{Tool: "Edit", Arg: "/abs/a.txt"}, "/w", both("/abs/a.txt")},
		{"absolute without cwd", agent.Rule{Tool: "Write", Arg: "/abs/a.txt"}, "", both("/abs/a.txt")},
		{"relative without cwd", agent.Rule{Tool: "Write", Arg: "a.txt"}, "", nil},
		{"read stays read_file", agent.Rule{Tool: "Read", Arg: "a.txt"}, "/w", []string{"read_file(/w/a.txt)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := agyRules(c.rule, c.cwd); !slices.Equal(got, c.want) {
				t.Errorf("agyRules(%s, %q) = %q, want %q", c.rule, c.cwd, got, c.want)
			}
		})
	}
}

// A Deny agy can't hold (no gate hook for a Bash deny, a glob path, a bare tool) or a failed guard load refuses.
func TestAgyPermissionSettingsFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, r := range []agent.Rule{{Tool: "Bash", Arg: "touch x"}, {Tool: "Bash", Arg: "rm:*"}, {Tool: "Edit", Arg: "internal/**"}, {Tool: "WebFetch"}} {
		_, _, err := agyPermissionSettings(agent.Permissions{Deny: []agent.Rule{r}}, true, true, "/w")
		if err == nil || !strings.Contains(err.Error(), r.String()) {
			t.Errorf("deny %s: err = %v, want a refusal naming it", r, err)
		}
	}
	orig := loadAgentConfig
	loadAgentConfig = func() (*agentconfig.Config, error) { return nil, errors.New("overlay unreadable") }
	t.Cleanup(func() { loadAgentConfig = orig })
	_, _, err := agyPermissionSettings(agent.Permissions{Mode: "auto"}, true, true, "/w")
	if err == nil || !strings.Contains(err.Error(), "overlay unreadable") {
		t.Errorf("failed guard load: err = %v, want it returned", err)
	}
}

// Plan mode is refused where agy cannot deny unmatched calls
func TestAgyPermissionSettingsRefusesPlan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.Permissions{Mode: "plan"}
	_, _, err := agyPermissionSettings(p, true, true, "/w")
	var ae *retry.Error
	if !errors.As(err, &ae) || ae.Kind != retry.KindFatal || !strings.Contains(err.Error(), "drop --tools") {
		t.Errorf("plan with tools: err = %v, want a fatal refusal naming the remedy", err)
	}
	if _, _, err := agyPermissionSettings(p, true, false, "/w"); err == nil || !strings.Contains(err.Error(), "read_file") {
		t.Errorf("plan without tools or gate hook: err = %v, want a refusal naming read_file", err)
	}
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agyPermissionSettings(p, true, false, "/w"); err != nil {
		t.Errorf("plan without tools, gate installed: %v", err)
	}
}

// With the gate hook installed a Bash Deny agy cannot express natively is the hook's; exact ones also ride native.
func TestAgyBashDenyNeedsGateHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.Permissions{Deny: []agent.Rule{{Tool: "Bash", Arg: "rm *"}, {Tool: "Bash", Arg: "touch x"}}}
	if _, _, err := agyPermissionSettings(p, true, true, "/w"); err == nil || !strings.Contains(err.Error(), "Bash(rm *)") {
		t.Fatalf("no gate installed: err = %v, want a refusal naming Bash(rm *)", err)
	}
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	got, _, err := agyPermissionSettings(p, true, true, "/w")
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
			_, _, err = agyPermissionSettings(p, true, true, cwd)
			if (err == nil) != c.wantGate {
				t.Fatalf("Bash deny with %s hook: err = %v, want gate accepted = %v", c.name, err, c.wantGate)
			}
		})
	}
}

// --read-only on agy is the bare Bash/Write/Edit denies: refused without the gate hook, the hook's alone with it.
func TestAgyReadOnlyDenyNeedsGateHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.Permissions{Deny: agent.ReadOnlyDeny}
	_, _, err := agyPermissionSettings(p, true, true, "/w")
	for _, r := range []string{"Bash", "Write", "Edit"} { // literal: the read-only set itself is under test
		if err == nil || !strings.Contains(err.Error(), r) {
			t.Errorf("no gate installed: err = %v, want a refusal naming %s", err, r)
		}
	}
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	got, _, err := agyPermissionSettings(p, true, true, "/w")
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

// hookSets is a hooks.json with one disabled set holding off and one enabled set holding on.
func hookSets(t *testing.T, off, on []settings.MatcherGroup) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"off": map[string]any{"enabled": false, "PreToolUse": off}, "on": map[string]any{"PreToolUse": on},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A malformed or unreadable hooks.json grants the gate on no tool; only a valid covering file does.
func TestAgyGateCoversFailsClosedOnBadHooksFile(t *testing.T) {
	command := (settings.EventSpec{Event: settings.EventPreToolUse, Name: "permission-gate"}).Command()
	group := settings.MatcherGroup{Hooks: []settings.Hook{{Type: "command", Command: command}}}
	valid, err := json.Marshal(map[string]any{"harness": map[string]any{"PreToolUse": []settings.MatcherGroup{group}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		content []byte
		mode    os.FileMode
		want    bool
	}{
		{"malformed", []byte("{not json"), 0o600, false},
		{"unreadable", valid, 0o000, false},
		{"valid", valid, 0o600, true},
		{"gate only in the disabled set", hookSets(t, []settings.MatcherGroup{group}, nil), 0o600, false},
		{"disabled set does not hide an enabled one", hookSets(t, []settings.MatcherGroup{group}, []settings.MatcherGroup{group}), 0o600, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "unreadable" && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("mode 0000 is still readable here")
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := agyHooksPath(home)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, c.content, c.mode); err != nil {
				t.Fatal(err)
			}
			for _, tool := range []string{"run_command", "read_file", "write_file"} {
				if got := agyGateCovers("", tool); got != c.want {
					t.Errorf("agyGateCovers(%q) = %v, want %v", tool, got, c.want)
				}
			}
		})
	}
}

// A hook set with "enabled": false is not enforcement; absent or true still is.
func TestAgyGateCoversIgnoresDisabledSet(t *testing.T) {
	command := (settings.EventSpec{Event: settings.EventPreToolUse, Name: "permission-gate"}).Command()
	group := settings.MatcherGroup{Hooks: []settings.Hook{{Type: "command", Command: command}}}
	for _, c := range []struct {
		name    string
		enabled any
		want    bool
	}{
		{"disabled", false, false},
		{"enabled", true, true},
		{"absent", nil, true},
		{"non-boolean", "no", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			set := map[string]any{"PreToolUse": []settings.MatcherGroup{group}}
			if c.enabled != nil {
				set["enabled"] = c.enabled
			}
			b, err := json.Marshal(map[string]any{"harness": set})
			if err != nil {
				t.Fatal(err)
			}
			path := agyHooksPath(home)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if got := agyGateCovers("", "run_command"); got != c.want {
				t.Errorf("agyGateCovers = %v, want %v", got, c.want)
			}
			_, _, err = agyPermissionSettings(agent.Permissions{Deny: agent.ReadOnlyDeny}, true, true, "/w")
			if (err == nil) != c.want {
				t.Errorf("read-only deny err = %v, want accepted = %v", err, c.want)
			}
		})
	}
}

// Only useGuardRules "false" drops the guard denies from --settings; any other value keeps them.
func TestAgyGuardRulesFromExtras(t *testing.T) {
	var tpl struct{ Permissions settings.PermissionLists }
	if err := json.Unmarshal(settings.Template(), &tpl); err != nil {
		t.Fatal(err)
	}
	fallback := mergeAgyDeny(nil, agyGuardRules(tpl.Permissions.Deny, io.Discard))
	for _, c := range []struct {
		name, body, guard string
		want              []string
	}{
		{"live deny honored", `{"permissions":{"deny":["Bash(owner-command)"]}}`, "", []string{"command(owner-command)"}},
		{"template rules omitted", `{"permissions":{"deny":[]}}`, "true", nil},
		{"guard disabled", `{"permissions":{"deny":["Bash(owner-command)"]}}`, "false", nil},
		{"missing file", "", "", fallback},
		{"invalid file", "{", "", fallback},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := agenttest.Scratch(t)
			if c.body != "" {
				if err := os.WriteFile(settings.SettingsFile(), []byte(c.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			s := agent.Settings{Agent: "gemini", Headless: true, Tools: true, WorkDir: home,
				Permissions: agent.Permissions{Mode: "bypassPermissions"},
				Record:      vault.AgentRecord{Extras: map[string]string{extraUseGuardRules: c.guard}}}
			_, argv, _, cleanup, err := geminiEngine{agent.NewBaseEngine("gemini")}.RenderSettings(t.Context(), s, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			cleanup()
			i := slices.Index(argv, "--settings")
			if i < 0 {
				t.Fatalf("argv %q lacks --settings", argv)
			}
			var doc struct{ Permissions settings.PermissionLists }
			if err := json.Unmarshal([]byte(argv[i+1]), &doc); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(doc.Permissions.Deny, c.want) {
				t.Errorf("deny = %v, want %v", doc.Permissions.Deny, c.want)
			}
		})
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
	overlay := `{"guardRules":{"deny":["Bash(git push --force:*)","Bash(rm -rf /)"]}}`
	vaulttest.Put(t, vault.Overlay, "agents", overlay)

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
	for _, want := range []string{`"mine"`, `HARNESS_HOOK_AGENT=gemini $HOME/.claude/bin/claude-code hook PreToolUse --safety-scan`, `HARNESS_HOOK_AGENT=gemini $HOME/.claude/bin/claude-code hook PreToolUse --permission-gate`, `"matcher": "run_command|read_file|write_file|edit_file"`} {
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

// Edit and Write path rules render both native write entries, in Deny and Allow alike.
func TestAgyWriteRulesRenderBothNativeTools(t *testing.T) {
	p := agent.Permissions{
		Deny:  []agent.Rule{{Tool: "Edit", Arg: "d.txt"}, {Tool: "Write", Arg: "w.txt"}},
		Allow: []agent.Rule{{Tool: "Edit", Arg: "/abs/a.txt"}},
	}
	got, _, err := agyPermissionSettings(p, false, true, "/w")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Permissions struct{ Allow, Deny []string }
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"write_file(/w/d.txt)", "edit_file(/w/d.txt)", "write_file(/w/w.txt)", "edit_file(/w/w.txt)"} {
		if !slices.Contains(doc.Permissions.Deny, want) {
			t.Errorf("deny %v lacks %q", doc.Permissions.Deny, want)
		}
	}
	for _, want := range []string{"write_file(/abs/a.txt)", "edit_file(/abs/a.txt)"} {
		if !slices.Contains(doc.Permissions.Allow, want) {
			t.Errorf("allow %v lacks %q", doc.Permissions.Allow, want)
		}
	}
}

// A tool step agy refused for want of a grant yields the exact --allow, and that rule renders natively
func failedAgyStep(t *testing.T, tool, params, msg string) agyEvent {
	t.Helper()
	var ev agyEvent
	line := `{"event":"step_update","step_update":{"step_type":"tool","state":"ERROR","tool_name":"` + tool +
		`","tool_info":{"parameters":` + params + `,"error":{"message":"` + msg + `"}}}}`
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestAgyRefusedCallRemedy(t *testing.T) {
	step := func(tool, params, msg string) agyEvent { return failedAgyStep(t, tool, params, msg) }
	missed := "permission check failed: user denied permission to run command"
	call, ok := agyRefusedCall(step("run_command", `{"CommandLine":"echo zebra-ran"}`, missed))
	if !ok {
		t.Fatal("an allow miss is not recognised as a refused call")
	}
	d := &agyDenier{cwd: "/w"}
	want := "agy: refused Bash(echo zebra-ran): retry with --allow 'Bash(echo zebra-ran)'"
	if got := d.notes([]agent.Rule{call}); !slices.Equal(got, []string{want}) {
		t.Fatalf("notes = %q, want %q", got, want)
	}
	if got, _, err := agyPermissionSettings(agent.Permissions{Allow: []agent.Rule{call}}, false, false, "/w"); err != nil || !strings.Contains(got, `"command(echo zebra-ran)"`) {
		t.Errorf("the offered allow renders %q, err %v; want it as a native command rule", got, err)
	}
	if _, ok := agyRefusedCall(step("run_command", `{"CommandLine":"x"}`, "Matches user-configured deny rule.")); ok {
		t.Error("a deny rule's refusal was treated as an allow miss")
	}
	if _, ok := agyRefusedCall(step("search_web", `{"Query":"q"}`, missed)); ok {
		t.Error("a tool with no rule spelling produced a call")
	}
	denied := &agyDenier{p: agent.Permissions{Deny: []agent.Rule{{Tool: "Bash", Arg: "echo zebra-ran"}}}, cwd: "/w"}
	glob := agent.Rule{Tool: "Bash", Arg: "echo *"}
	checkNoAllowOffered(t, map[string]struct {
		d    *agyDenier
		call agent.Rule
	}{"deny": {denied, call}, "glob": {d, glob}})
}

// checkNoAllowOffered expects each denier's note for its call to offer no --allow remedy.
func checkNoAllowOffered(t *testing.T, cases map[string]struct {
	d    *agyDenier
	call agent.Rule
}) {
	t.Helper()
	for name, c := range cases {
		if got := c.d.notes([]agent.Rule{c.call}); len(got) != 1 || !strings.Contains(got[0], "no allow offered") {
			t.Errorf("%s: notes = %q, want a note with no allow", name, got)
		}
	}
}
