package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// describe must cover every listed verb, name the registered hooks, and marshal without a credential.
func TestDescribeCoversVerbsAndHooks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	d, err := describe(reg)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range d.Commands {
		names[c.Name] = true
	}
	for _, want := range []string{"ask", "doctor", "describe", "profile", "memory"} {
		if !names[want] {
			t.Errorf("describe lacks verb %q", want)
		}
	}
	if len(d.Hooks) == 0 || len(d.Runners) == 0 || len(d.Providers.List) == 0 {
		t.Fatalf("empty section: hooks=%d runners=%d providers=%d", len(d.Hooks), len(d.Runners), len(d.Providers.List))
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"sk-", "ghp_", "Bearer "} {
		if strings.Contains(string(b), leak) {
			t.Errorf("describe JSON carries a credential-shaped value %q", leak)
		}
	}
}

// flagNames merges a verb's own flags with its subverbs' and renders value-taking ones with a placeholder.
func TestFlagNamesMergesSubverbs(t *testing.T) {
	got := strings.Join(flagNames(specTree()["memory"]), " ")
	for _, want := range []string{"--repo <value>", "--notice", "--session <value>"} {
		if !strings.Contains(got, want) {
			t.Errorf("flagNames(memory) = %q, missing %q", got, want)
		}
	}
}
