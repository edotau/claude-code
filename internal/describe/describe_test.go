package describe

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func fixtureVerbs() []Verb {
	memory := &helptext.Node{Sub: map[string]*helptext.Node{
		"update": {Flags: []helptext.Flag{{Name: "session"}, {Name: "notice", IsBool: true}}},
		"recall": {Flags: []helptext.Flag{{Name: "repo"}}},
	}}
	return []Verb{
		{Name: "memory", Group: "SESSION", Summary: "bank", Node: memory},
		{Name: "doctor", Group: "DIAGNOSTICS", Summary: "checks"},
		{Name: "__complete", Hidden: true, Summary: "machine"},
	}
}

// Build lists each verb with its merged flags, skips "__" machine verbs, and marshals without a credential.
func TestBuildCoversVerbsAndHooks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Build(reg, fixtureVerbs())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Commands) != 2 || d.Commands[0].Name != "memory" || d.Commands[1].Name != "doctor" {
		t.Fatalf("commands = %+v, want memory, doctor (no __complete)", d.Commands)
	}
	if got := strings.Join(d.Commands[0].Subverbs, " "); got != "recall update" {
		t.Errorf("memory subverbs = %q", got)
	}
	if len(d.Hooks) == 0 || len(d.Providers.List) == 0 {
		t.Fatalf("empty section: hooks=%d providers=%d", len(d.Hooks), len(d.Providers.List))
	}
	checkNoCredentials(t, d)
}

func checkNoCredentials(t *testing.T, d any) {
	t.Helper()
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

// FlagNames merges a verb's own flags with its subverbs' and renders value-taking ones with a placeholder.
func TestFlagNamesMergesSubverbs(t *testing.T) {
	got := strings.Join(FlagNames(fixtureVerbs()[0].Node), " ")
	for _, want := range []string{"--repo <value>", "--notice", "--session <value>"} {
		if !strings.Contains(got, want) {
			t.Errorf("FlagNames(memory) = %q, missing %q", got, want)
		}
	}
}
