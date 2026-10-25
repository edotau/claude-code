package cli

import (
	"encoding/base64"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/vault"
)

const acmeProvider = `"providers":{"acme":{"kind":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}}`

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

func TestApplyProfileRow(t *testing.T) {
	cases := []struct {
		name                          string
		active, agent                 string
		prov, model, eff              string
		wantProv, wantModel, wantEffo string
	}{
		{"no profile", "", "codex", "", "", "", "", "", ""},
		{"row fills all", "work", "codex", "", "", "", "acme", "acme-large", "high"},
		{"no row for agent", "work", "api", "", "", "", "", "", ""},
		{"explicit provider wins, row model skipped", "work", "codex", "openai", "", "", "openai", "", "high"},
		{"explicit model wins", "work", "codex", "", "mine", "", "acme", "mine", "high"},
		{"provider:model form left alone", "work", "codex", "", "openai:gpt-5", "", "", "openai:gpt-5", "high"},
		{"explicit effort wins", "work", "codex", "", "", "low", "acme", "acme-large", "low"},
		{"provider equal to account takes row model", "work", "codex", "acme", "", "", "acme", "acme-large", "high"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			profileFixture(t, c.active)
			p, m, e := c.prov, c.model, c.eff
			applyProfileRow(c.agent, &p, &m, &e)
			if p != c.wantProv || m != c.wantModel || e != c.wantEffo {
				t.Fatalf("got (%q,%q,%q), want (%q,%q,%q)", p, m, e, c.wantProv, c.wantModel, c.wantEffo)
			}
		})
	}
}

func TestResolveAskChainHubNativeFallback(t *testing.T) {
	profileFixture(t, "")
	// acme has no opencodeID: the native runner runs alone, never the hub leg.
	r, chain, err := resolveAskChain("opencode", "acme", "", "p", agent.FallbackVendors, false, "")
	if err != nil {
		t.Fatalf("resolveAskChain(acme) = %v", err)
	}
	if len(chain.Legs) != 1 || chain.Legs[0].Runner != r {
		t.Fatalf("acme: want the native runner alone, got %+v", chain.Legs)
	}
	// chatgpt has opencodeID: it still goes through the hub leg.
	r, chain, err = resolveAskChain("opencode", "chatgpt", "", "p", agent.FallbackVendors, false, "")
	if err != nil {
		t.Fatalf("resolveAskChain(chatgpt) = %v", err)
	}
	if len(chain.Legs) != 1 || chain.Legs[0].Runner == r {
		t.Fatalf("chatgpt: want the hub leg, got the native runner")
	}
}
