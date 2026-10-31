package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const astra = "gpt-6-astra"

func astraOpts() router.PickOpts {
	return router.PickOpts{Configured: func(string, router.Cell) error { return nil }}
}

// stubDispatches swaps every dispatch's runner for a capturing stub; stubs[i] backs c.Dispatches[i].
func stubDispatches(c *Chain) []*pinFallbackRunner {
	stubs := make([]*pinFallbackRunner, len(c.Dispatches))
	for i := range c.Dispatches {
		f := &pinFallbackRunner{name: c.Dispatches[i].Agent.Name(), res: Result{Answer: "answer from " + c.Dispatches[i].Agent.Name()}}
		stubs[i], c.Dispatches[i].Agent = f, f
	}
	return stubs
}

func TestAstraModelIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("own_login", func(t *testing.T) {
		vaulttest.UseKeyring(t)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("CLAUDE_PROFILE", "")
		c, _, err := RouteChainFor(ctx, "codex", astra, "sonnet", router.FallbackAgents, false, astraOpts())
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Dispatches) == 0 || c.Dispatches[0].Agent.Name() != "codex" {
			t.Fatalf("first dispatch is not codex: %+v; skipped %v", c.Dispatches, c.Skipped)
		}
		stubs := stubDispatches(&c)
		if _, err := RunWithFallback(ctx, c, RunOptions{Prompt: "x", Model: astra}, Discard, nil); err != nil {
			t.Fatal(err)
		}
		if len(stubs[0].seen) != 1 {
			t.Fatalf("codex requests = %d, want 1", len(stubs[0].seen))
		}
		got := stubs[0].seen[0].Model
		if got != astra {
			t.Errorf("codex runner model = %q, want %q", got, astra)
		}
		if c.Dispatches[0].Model != got {
			t.Errorf("codex Dispatch.Model = %q but runner got %q: planned dispatch records the stale tier default, not the model run", c.Dispatches[0].Model, got)
		}
	})

	t.Run("zero_legs", func(t *testing.T) {
		stub := &pinFallbackRunner{name: "codex"}
		_, err := RunWithFallback(ctx, Chain{Want: "codex"}, RunOptions{Prompt: "x", Model: astra}, Discard, nil)
		var e *Error
		if !errors.As(err, &e) || e.Kind != KindFatal {
			t.Errorf("err = %v, want *Error KindFatal", err)
		}
		if len(stub.seen) != 0 {
			t.Errorf("runner calls = %d, want 0", len(stub.seen))
		}
	})
}

// astraProfile installs a signed-in Databricks session profile (sonnet -> gpt-6-1-sol) with menu as its opencode menu.
func astraProfile(t *testing.T, profile string, menu []providers.OpencodeModel) *providers.Provider {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", profile)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"`+profile+`":{"preset":"databricks",`+
		`"host":"https://profile.example.com","auth":{"type":"pat"},"models":{"sonnet":"gpt-6-1-sol"}}}}}`)
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers[profile]
	if p == nil {
		t.Fatal("profile absent")
	}
	vaulttest.Secret(t, providers.AuthSecretName(p), "profile-auth")
	return p
}

// profileCodexDispatches are the codex dispatches pinned to profile.
func profileCodexDispatches(c Chain, profile string) []Dispatch {
	var out []Dispatch
	for _, disp := range c.Dispatches {
		if disp.Agent.Name() == "codex" && strings.HasPrefix(disp.Model, profile+":") {
			out = append(out, disp)
		}
	}
	return out
}

func TestAstraProfileEligibility(t *testing.T) {
	ctx := context.Background()

	t.Run("supported_profile_retained", func(t *testing.T) {
		const profile = "astra-ok-q4"
		p := astraProfile(t, profile, []providers.OpencodeModel{
			{ID: "gpt-6-1-sol", Label: "GPT-6.1 Sol", Responses: true},
			{ID: astra, Label: "GPT-6 Astra", Responses: true},
		})
		c, _, err := RouteChainFor(ctx, "codex", astra, "sonnet", router.FallbackAgents, false, astraOpts())
		if err != nil {
			t.Fatal(err)
		}
		want := profile + ":" + p.WireModel(astra)
		dispatches := profileCodexDispatches(c, profile)
		if len(dispatches) != 1 || dispatches[0].Model != want {
			t.Errorf("profile codex dispatches = %+v, want one at %q; skipped %v", dispatches, want, c.Skipped)
		}
		for _, s := range c.Skipped {
			if strings.HasPrefix(s, "codex: serves no "+astra) {
				t.Errorf("Responses-capable profile dropped by the exact-model cell check: %q", s)
			}
		}
	})

	t.Run("unsupported_profile_skipped", func(t *testing.T) {
		const profile = "astra-no-z8"
		astraProfile(t, profile, []providers.OpencodeModel{
			{ID: "gpt-6-1-sol", Label: "GPT-6.1 Sol", Responses: true},
			{ID: astra, Label: "GPT-6 Astra", Responses: false},
		})
		c, _, err := RouteChainFor(ctx, "codex", astra, "sonnet", router.FallbackAgents, false, astraOpts())
		if err != nil {
			t.Fatal(err)
		}
		if dispatches := profileCodexDispatches(c, profile); len(dispatches) != 0 {
			t.Errorf("profile without an astra Responses route kept codex dispatches: %+v", dispatches)
		}
		// RED if the skip text omits the profile: "codex: serves no gpt-6-astra" does not name the account.
		named := false
		for _, s := range c.Skipped {
			if strings.Contains(s, "codex") && strings.Contains(s, profile) {
				named = true
			}
		}
		if !named {
			t.Errorf("no skip names codex and profile %q: %v", profile, c.Skipped)
		}
	})
}
