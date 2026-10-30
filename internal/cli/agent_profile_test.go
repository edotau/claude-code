package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/edotau/claude-code/internal/hooks"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/vault"
)

const acmeProvider = `"providers":{"openai":{"acme":{"protocol":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}}}`

// profileFixture isolates the vault and seeds provider acme (no opencodeID) plus, when active is set, a "work" set.
func profileFixture(t *testing.T, active string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	doc := `{` + acmeProvider + `}`
	if active != "" {
		doc = `{` + acmeProvider + `,"profiles":{"active":"` + active + `","sets":{"work":{
		  "codex":{"account":"acme","model":"acme-large","effort":"high"}}}}}`
	}
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

func TestResolveAskChainHubNativeFallback(t *testing.T) {
	profileFixture(t, "")
	// acme has no opencodeID: the native runner runs alone, never the hub leg.
	r, chain, err := resolveAskChain(context.Background(), "opencode", "acme", "", "p", agent.FallbackAgents, false, "")
	if err != nil {
		t.Fatalf("resolveAskChain(acme) = %v", err)
	}
	if len(chain.Legs) != 1 || chain.Legs[0].Agent != r {
		t.Fatalf("acme: want the native runner alone, got %+v", chain.Legs)
	}
	// chatgpt has opencodeID: it still goes through the hub leg.
	r, chain, err = resolveAskChain(context.Background(), "opencode", "chatgpt", "", "p", agent.FallbackAgents, false, "")
	if err != nil {
		t.Fatalf("resolveAskChain(chatgpt) = %v", err)
	}
	if len(chain.Legs) != 1 || chain.Legs[0].Agent == r {
		t.Fatalf("chatgpt: want the hub leg, got the native runner")
	}
}

// A cancelled batch routes nothing: resolveAskChain returns the ctx error before any quota refresh.
func TestResolveAskChainHonorsCancellation(t *testing.T) {
	refreshes := 0
	prev := routeRefresh
	routeRefresh = func(context.Context, string) bool { refreshes++; return false }
	t.Cleanup(func() { routeRefresh = prev })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := resolveAskChain(ctx, "opencode", "", "", "p", agent.FallbackAgents, false, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if refreshes != 0 {
		t.Errorf("%d quota refreshes ran under a cancelled ctx", refreshes)
	}
}

// An explicit --profile with no --model runs the classifier's tier slot, as the pinned route does.
func TestResolveAskChainProfileTakesTierSlot(t *testing.T) {
	profileFixture(t, "")
	for _, profile := range []string{"chatgpt", "acme"} { // hub leg and native (no opencodeID)
		_, chain, err := resolveAskChain(context.Background(), "opencode", profile, "", "pong", agent.FallbackAgents, false, "")
		if err != nil || len(chain.Legs) != 1 {
			t.Fatalf("%s: chain = %+v, err %v", profile, chain, err)
		}
		if want := (hooks.RuleClassifier{}).Tier("pong"); chain.Legs[0].Model != want {
			t.Errorf("%s: leg model = %q, want tier %q", profile, chain.Legs[0].Model, want)
		}
	}
}
