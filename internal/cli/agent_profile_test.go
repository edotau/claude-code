package cli

import (
	"encoding/base64"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

const acmeProvider = `"providers":{"openai":{"acme":{"kind":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}}}`

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

// applyProfileRow fills an unset --profile from the agent's <AGENT>_PROFILE pin; a flag wins.
func TestApplyProfileRow(t *testing.T) {
	profileFixture(t, "")
	if err := providers.SetPins(map[string]string{providers.PinCodexProfile: "acme"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, agent, flag, want string }{
		{"pin fills", "codex", "", "acme"},
		{"flag wins", "codex", "openai", "openai"},
		{"no pin for agent", "gemini", "", ""},
		{"agent without a pin key", "claude", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, m, e := c.flag, "", ""
			applyProfileRow(c.agent, &p, &m, &e)
			if p != c.want || m != "" || e != "" {
				t.Fatalf("got (%q,%q,%q), want profile %q", p, m, e, c.want)
			}
		})
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
