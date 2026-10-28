package cli

import (
	"encoding/base64"
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
	r, chain, err := resolveAskChain("opencode", "acme", "", "p", agent.FallbackAgents, false, "")
	if err != nil {
		t.Fatalf("resolveAskChain(acme) = %v", err)
	}
	if len(chain.Legs) != 1 || chain.Legs[0].Agent != r {
		t.Fatalf("acme: want the native runner alone, got %+v", chain.Legs)
	}
	// chatgpt has opencodeID: it still goes through the hub leg.
	r, chain, err = resolveAskChain("opencode", "chatgpt", "", "p", agent.FallbackAgents, false, "")
	if err != nil {
		t.Fatalf("resolveAskChain(chatgpt) = %v", err)
	}
	if len(chain.Legs) != 1 || chain.Legs[0].Agent == r {
		t.Fatalf("chatgpt: want the hub leg, got the native runner")
	}
}
