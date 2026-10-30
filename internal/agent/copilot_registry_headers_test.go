package agent

import (
	"context"
	"testing"

	"github.com/edotau/claude-code/internal/agent/copilotreg"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

// Regression: upstream provider headers (credentials) must not reach the router-bound SDK providers.
func TestPrepareCopilotRegistryDropsUpstreamHeaders(t *testing.T) {
	t.Cleanup(WithCopilotOS("linux")) // the macOS refusal has its own test
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	oldD, oldE, oldM := copilotDiscover, copilotEnsureRouter, copilotMintToken
	t.Cleanup(func() { copilotDiscover, copilotEnsureRouter, copilotMintToken = oldD, oldE, oldM })
	copilotDiscover = func(context.Context, *providers.Registry, *providers.Provider) ([]copilotreg.Endpoint, error) {
		var ep copilotreg.Endpoint
		ep.ID, ep.Name = "databricks-claude-sonnet-4", "s"
		return []copilotreg.Endpoint{ep}, nil
	}
	copilotEnsureRouter = func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }
	copilotMintToken = func(context.Context, string, int) (string, error) { return "tok", nil }

	up := &providers.Provider{Name: "dbx", Headers: map[string]string{"X-Api-Key": "upstream-secret", "Authorization": "Bearer up"}}
	rt, err := prepareCopilotRegistry(context.Background(), providers.Target{Provider: up, Model: "databricks-claude-sonnet-4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.providers) == 0 {
		t.Fatal("no providers built")
	}
	for _, p := range rt.providers {
		if len(p.Headers) != 1 || p.Headers[proxy.AgentHeader] != "copilot" {
			t.Errorf("provider %s headers = %v, want only %s", p.Name, p.Headers, proxy.AgentHeader)
		}
	}
	if up.Headers["X-Api-Key"] != "upstream-secret" {
		t.Error("caller's provider was mutated")
	}
}
