package agent

import (
	"context"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestModelRow(t *testing.T) {
	cases := []struct{ in, tier, row, exact string }{
		{"opus", "opus", "claude-opus-5-5", ""},
		{"claude-sonnet-5.5", "sonnet", "claude-sonnet-5-5", ""},
		{"databricks-claude-haiku-5-5", "haiku", "claude-haiku-5-5", ""},
		{"gpt-6.1-sol", "", "", "gpt-6-1-sol"},
		{"glm-5.3", "", "", "glm-5-3"},
		{"no-such-model", "", "", ""},
	}
	for _, c := range cases {
		tier, row, exact := modelRow(c.in)
		if tier != c.tier || row != c.row || exact != c.exact {
			t.Errorf("modelRow(%q) = (%q, %q, %q), want (%q, %q, %q)", c.in, tier, row, exact, c.tier, c.row, c.exact)
		}
	}
}

func TestClaudeCellModelIsTheAlias(t *testing.T) {
	if got := claudeCellModel("claude-opus-5-5"); got != "opus" {
		t.Errorf("claudeCellModel(claude-opus-5-5) = %q, want opus (so pins resolve it)", got)
	}
	if got := claudeCellModel("claude-opus-4-1"); got != "claude-opus-4-1" {
		t.Errorf("an alias-less row stays canonical, got %q", got)
	}
}

// A canonical Claude id takes the session's alias selection, so a CLAUDE_<TIER>_MODEL pin wins over the typed id.
func TestResolveTargetCanonicalIDTakesTheTierPin(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"kind":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5-5"}}}}}`)
	if err := providers.SetPins(map[string]string{providers.SlotPin("sonnet"): "claude-sonnet-5"}); err != nil {
		t.Fatal(err)
	}
	_, got, err := resolveTarget(Request{Provider: "dev", Model: "claude-sonnet-5-5"})
	if err != nil || got.Model != "claude-sonnet-5" {
		t.Errorf("claude-sonnet-5-5 = %+v (%v), want the pinned claude-sonnet-5", got, err)
	}
}

// A non-Claude id is exact: codex (its cell serves gpt-6.1-sol) keeps a leg, claude and copilot get none.
func TestRouteChainForNonClaudeIDIsExact(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	all := router.Opts{Configured: func(string, router.Cell) error { return nil }}
	c, _, err := RouteChainFor(context.Background(), "codex", "gpt-6.1-sol", "sonnet", FallbackAll, false, all)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Legs) == 0 || c.Legs[0].Agent.Name() != "codex" || !c.Legs[0].keepCaller {
		t.Fatalf("legs = %+v, want codex first and keeping the caller's id", c.Legs)
	}
	for _, l := range c.Legs[1:] {
		if n := l.Agent.Name(); n == "claude" || n == "copilot" {
			t.Errorf("%s got a leg for a model its cell does not serve", n)
		}
	}
}

// An alias-less older row runs its own cell on every agent, not the current tier's.
func TestRouteChainForCanonicalRowUsesItsCell(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	all := router.Opts{Configured: func(string, router.Cell) error { return nil }}
	c, _, err := RouteChainFor(context.Background(), "copilot", "claude-sonnet-5.5", "", FallbackAll, false, all)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range c.Legs {
		if l.Agent.Name() == "claude" && l.Model != "sonnet" {
			t.Errorf("claude leg model = %q, want the sonnet alias", l.Model)
		}
	}
}

func TestCopilotModelIDIsDotted(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5-5[1m]": "claude-opus-5.5", "claude-opus-4.5": "claude-opus-4.5", "gpt-5.3-codex": "gpt-5.3-codex",
	} {
		if got := copilotModelID(in); got != want {
			t.Errorf("copilotModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

// A run on an agent's own login reports the registry profile naming that login, never the retired "own-login".
func TestOwnProfileNamesTheLoginRow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got := ownProfile("codex"); got == "own-login" || got == "" {
		t.Errorf("ownProfile(codex) = %q", got)
	}
	if got := ownProfile("opencode"); got != "opencode" {
		t.Errorf("ownProfile(opencode) = %q, want the agent name when no row names its login", got)
	}
}
