package client

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// RunOptions.Lead must reach the router pick: a lead naming no registry row is skipped by name.
func TestClientRouteCarriesLeadToThePick(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
	c := NewClient(router.RuleClassifier{}.Alias, nil, nil)
	chain, err := c.route(t.Context(), nil, agent.RunOptions{Agent: "codex", Prompt: "x", Lead: "no-such-row", Fallback: router.FallbackAll})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(chain.Skipped, "\n"), "no-such-row") {
		t.Fatalf("skipped = %q, want a skip naming the lead row no-such-row", chain.Skipped)
	}
}
