package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/settings"
)

// Glob-free path denies and exact allows map to agy rules; Ask rides as allow; unmapped Allow/Ask are reported.
func TestAgyPermissionSettings(t *testing.T) {
	p := agent.Permissions{
		Deny:  []agent.Rule{{Tool: "Write", Arg: "denied2.txt"}, {Tool: "Read", Arg: "/abs/secret"}},
		Allow: []agent.Rule{{Tool: "Bash", Arg: "go test ./..."}, {Tool: "Bash", Arg: "git log:*"}},
		Ask:   []agent.Rule{{Tool: "WebFetch"}},
	}
	got, unmapped, err := agyPermissionSettings(p, true, "/w", io.Discard)
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
	if s, _, _ := agyPermissionSettings(agent.Permissions{}, true, "/w", io.Discard); s != "" {
		t.Errorf("zero policy renders %q, want no --settings", s)
	}
}

// A Deny agy can't hold (a Bash deny with no gate hook installed, a glob path, a bare tool) or a failed guard load refuses the dispatch.
func TestAgyPermissionSettingsFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, r := range []agent.Rule{{Tool: "Bash", Arg: "touch x"}, {Tool: "Bash", Arg: "rm:*"}, {Tool: "Edit", Arg: "internal/**"}, {Tool: "WebFetch"}} {
		_, _, err := agyPermissionSettings(agent.Permissions{Deny: []agent.Rule{r}}, true, "/w", io.Discard)
		if err == nil || !strings.Contains(err.Error(), r.String()) {
			t.Errorf("deny %s: err = %v, want a refusal naming it", r, err)
		}
	}
	orig := loadAgentConfig
	loadAgentConfig = func() (*agentconfig.Config, error) { return nil, errors.New("overlay unreadable") }
	t.Cleanup(func() { loadAgentConfig = orig })
	_, _, err := agyPermissionSettings(agent.Permissions{Mode: "auto"}, true, "/w", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "overlay unreadable") {
		t.Errorf("failed guard load: err = %v, want it returned", err)
	}
}

// Plan mode with tools on is refused as fatal (agy cannot deny unmatched calls); without tools nothing can run.
func TestAgyPermissionSettingsRefusesPlan(t *testing.T) {
	p := agent.Permissions{Mode: "plan"}
	_, _, err := agyPermissionSettings(p, true, "/w", io.Discard)
	var ae *retry.Error
	if !errors.As(err, &ae) || ae.Kind != retry.KindFatal || !strings.Contains(err.Error(), "plan") {
		t.Errorf("plan with tools: err = %v, want a fatal refusal naming plan", err)
	}
	if _, _, err := agyPermissionSettings(p, false, "/w", io.Discard); err != nil {
		t.Errorf("plan without tools: %v", err)
	}
}

// With the gate hook installed a Bash Deny agy cannot express natively is the hook's: accepted, exact ones also ride native.
func TestAgyBashDenyNeedsGateHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.Permissions{Deny: []agent.Rule{{Tool: "Bash", Arg: "rm *"}, {Tool: "Bash", Arg: "touch x"}}}
	if _, _, err := agyPermissionSettings(p, true, "/w", io.Discard); err == nil || !strings.Contains(err.Error(), "Bash(rm *)") {
		t.Fatalf("no gate installed: err = %v, want a refusal naming Bash(rm *)", err)
	}
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	got, _, err := agyPermissionSettings(p, true, "/w", io.Discard)
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
			_, _, err = agyPermissionSettings(p, true, cwd, io.Discard)
			if (err == nil) != c.wantGate {
				t.Fatalf("Bash deny with %s hook: err = %v, want gate accepted = %v", c.name, err, c.wantGate)
			}
		})
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
	for _, want := range []string{`"mine"`, `HARNESS_HOOK_AGENT=gemini $HOME/.claude/bin/claude-code hook PreToolUse --permission-gate`, `"matcher": "run_command|read_file|write_file|edit_file"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("hooks.json lacks %s:\n%s", want, b)
		}
	}
}

// stagedHooks renders the agy hooks.json under home and applies the staged write.
func stagedHooks(home string) (bool, error) {
	var plan render.RenderPlan
	changed, err := renderGeminiHooks(home, render.RenderOptions{}.Stage(&plan), io.Discard)
	if err != nil {
		return changed, err
	}
	return changed, plan.Apply()
}

// --read-only on agy is the bare Bash/Write/Edit denies: refused without the gate hook, the hook's alone once it is installed.
func TestAgyReadOnlyDenyNeedsGateHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.Permissions{Deny: agent.ReadOnlyDeny}
	_, _, err := agyPermissionSettings(p, true, "/w", io.Discard)
	for _, r := range []string{"Bash", "Write", "Edit"} { // literal: the read-only set itself is under test
		if err == nil || !strings.Contains(err.Error(), r) {
			t.Errorf("no gate installed: err = %v, want a refusal naming %s", err, r)
		}
	}
	if _, err := stagedHooks(home); err != nil {
		t.Fatal(err)
	}
	got, _, err := agyPermissionSettings(p, true, "/w", io.Discard)
	if err != nil || strings.Contains(got, "()") || strings.Contains(got, "write_file") {
		t.Fatalf("gate installed: settings %q err %v, want accepted with no native rule for a bare tool", got, err)
	}
}
