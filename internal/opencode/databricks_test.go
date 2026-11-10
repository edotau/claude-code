package opencode

import (
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func regressionDatabricksProfile(t *testing.T, name string, menu []providers.OpencodeModel) *providers.Provider {
	t.Helper()
	vaulttest.Use(t)
	overlay := fmt.Sprintf(`{"providers":{"databricks":{%q:{"preset":"databricks","host":"https://workspace.example.test"}}}}`, name)
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Row(name)
	if p == nil {
		t.Fatalf("missing test profile %q", name)
	}
	return p
}

func regressionGeneratedMenu(t *testing.T, p *providers.Provider, model string) []byte {
	t.Helper()
	env, _, err := opencodeDatabricks(p, "harness-"+p.Name, providers.ProtocolOpenAI, p.BaseURL, agentconfig.MenuWire(p, model), "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		if body, ok := strings.CutPrefix(kv, opencodeConfigEnv+"="); ok {
			return []byte(body)
		}
	}
	t.Fatal("generated launch has no opencode config")
	return nil
}
