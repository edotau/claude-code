package agent

import (
	"context"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A codex dispatch asked at sonnet keeps the alias in the record (and so in --retry) and never inherits opus's effort.
func TestRequestedAliasSurvivesSharedModel(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	effortAt := func(alias string) string {
		c, _, err := RouteChainFor(context.Background(), "codex", alias, "sonnet", FallbackNone, false, astraOpts())
		if err != nil || len(c.Legs) == 0 || c.Legs[0].Agent.Name() != "codex" {
			t.Fatalf("%s chain = %+v, err %v", alias, c, err)
		}
		if c.Legs[0].Model != "gpt-6.1-sol" {
			t.Fatalf("%s leg model = %q, want the shared gpt-6.1-sol", alias, c.Legs[0].Model)
		}
		return c.Legs[0].Effort
	}
	if got := effortAt("opus"); got != "high" {
		t.Fatalf("opus control effort = %q, want high", got)
	}
	if got := effortAt("sonnet"); got != "" {
		t.Errorf("sonnet effort = %q, must not be upgraded to opus's high", got)
	}

	rec := NewRecord("codex", RunOptions{Prompt: "p", Model: "sonnet"}, time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC))
	rec.Finish(Result{Answer: "ok", Model: "gpt-6.1-sol"}, nil, time.Second)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Request.Model != "sonnet" || got.Model != "gpt-6.1-sol" {
		t.Errorf("record request %q model %q: the replay source must stay the alias", got.Request.Model, got.Model)
	}
}
