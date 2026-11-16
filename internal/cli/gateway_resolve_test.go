package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

func TestRouteLine(t *testing.T) {
	r := agent.ModelRoute{Agent: "copilot", Account: "github·business", Model: "gpt-6-astra", Source: "prefix"}
	if got := routeLine(r); got != "copilot · github·business · gpt-6-astra (prefix)" {
		t.Errorf("routeLine = %q", got)
	}
}

func TestGatewayResolveUsage(t *testing.T) {
	if code := cmdGatewayResolve([]string{"claude", "--live"}); code != 2 {
		t.Errorf("--live without --all-profiles exit = %d; want 2", code)
	}
}

// An untyped resolve reports the hop RunQuery picks (the label's resolver), its note kept; an error is returned.
func TestResolveFirstHop(t *testing.T) {
	base, err := os.ReadFile("../providers/defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Parse(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got agent.ModelQuery
	stub := func(_ context.Context, _ *providers.Registry, q agent.ModelQuery, _ string) (agent.ModelQuery, string, error) {
		got = q
		return agent.ModelQuery{Agent: "claude", Model: "ultra:claude-sonnet-5-5"}, "codex is gated out", nil
	}
	old := resolveRunQuery
	t.Cleanup(func() { resolveRunQuery = old })
	resolveRunQuery = stub
	r, err := resolveFirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"})
	if err != nil || got.Agent != "codex" || r.Agent != "claude" || r.Model != "claude-sonnet-5-5" || !strings.Contains(r.Note, "gated out") {
		t.Errorf("resolveFirstHop = %+v, %v (query %+v); want the stubbed claude sonnet hop with its note", r, err, got)
	}
	resolveRunQuery = func(context.Context, *providers.Registry, agent.ModelQuery, string) (agent.ModelQuery, string, error) {
		return agent.ModelQuery{}, "", errors.New("no route runs codex")
	}
	if _, err := resolveFirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"}); err == nil {
		t.Error("resolveFirstHop swallowed the ladder error")
	}
}
