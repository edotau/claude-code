package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
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

// A Deny agy can't hold (any Bash deny, a glob path, a bare tool) or a failed guard load refuses the dispatch.
func TestAgyPermissionSettingsFailsClosed(t *testing.T) {
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
