package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
	hint := ""
	stub := func(_ context.Context, _ *providers.Registry, q agent.ModelQuery, alias string) (agent.ModelQuery, string, error) {
		got, hint = q, alias
		return agent.ModelQuery{Agent: "claude", Model: "ultra:claude-sonnet-5-5"}, "codex is gated out", nil
	}
	old := resolveRunQuery
	t.Cleanup(func() { resolveRunQuery = old })
	resolveRunQuery = stub
	r, err := resolveFirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"}, "fix the typo")
	if err != nil || got.Agent != "codex" || hint != "haiku" || r.Agent != "claude" || r.Model != "claude-sonnet-5-5" || !strings.Contains(r.Note, "gated out") {
		t.Errorf("resolveFirstHop = %+v, %v (query %+v); want the stubbed claude sonnet hop with its note", r, err, got)
	}
	resolveRunQuery = func(context.Context, *providers.Registry, agent.ModelQuery, string) (agent.ModelQuery, string, error) {
		return agent.ModelQuery{}, "", errors.New("no route runs codex")
	}
	if _, err := resolveFirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"}, ""); err == nil {
		t.Error("resolveFirstHop swallowed the ladder error")
	}
}

// --task resolves an untyped model to the brief's tier, the same pick the run and the label make.
func TestGatewayResolveTaskTier(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	dir := t.TempDir()
	out := map[string]string{}
	for name, brief := range map[string]string{"fast": "Fix the typo in a.go.", "deep": "Audit the safety-scan rules."} {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte(brief), 0o600); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
			return cmdGatewayResolve([]string{"opencode", "--profile", "dev", "--task", f})
		})
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", name, code, stderr)
		}
		out[name] = stdout
	}
	if out["fast"] == out["deep"] {
		t.Errorf("a typo fix and an audit resolve the same: %q", out["fast"])
	}
}
