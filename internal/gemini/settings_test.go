package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
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
			t.Setenv("HOME", t.TempDir())
			cwd := t.TempDir()
			path := agyHooksPath()
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
