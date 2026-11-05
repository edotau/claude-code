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

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/render"
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
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Kind != agent.KindFatal || !strings.Contains(err.Error(), "plan") {
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
