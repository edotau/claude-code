package agent

import (
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// gwRow is a Databricks ai-gateway row: anthropic + responses routes, no openai route.
func gwRow(name string) *providers.Provider {
	h := "https://h.example.com"
	return &providers.Provider{
		Name: name, Preset: providers.PresetDatabricks, ModelPrefix: "system.ai.",
		Protocol: providers.ProtocolAnthropic, BaseURL: h + "/ai-gateway/anthropic",
		Routes: map[string]string{providers.ProtocolResponses: h + "/ai-gateway/codex/v1"},
	}
}


// configureRoute surfaces Serves' reason verbatim, prefixed by the agent.
func TestConfigureRouteUsesServesReason(t *testing.T) {
	vaulttest.Use(t)
	cases := []struct {
		agent string
		p     *providers.Provider
	}{
		{"gemini", &providers.Provider{Name: "ant", Protocol: providers.ProtocolAnthropic, BaseURL: "https://a", Auth: providers.Auth{Type: providers.AuthKey}}},
		{"codex", &providers.Provider{Name: "ant", Protocol: providers.ProtocolAnthropic, BaseURL: "https://a", Auth: providers.Auth{Type: providers.AuthKey}}},
		{"copilot", &providers.Provider{Name: "sub", Protocol: providers.ProtocolAnthropic, BaseURL: "https://s", Auth: providers.Auth{Type: providers.AuthCLI}}},
	}
	for _, c := range cases {
		_, why := Serves(c.agent, c.p, "")
		if why == "" {
			t.Fatalf("%s/%s: control: Serves has no reason", c.agent, c.p.Name)
		}
		_, _, err := schemaFor(c.agent).configureRoute(t.Context(), providers.Target{Provider: c.p, Model: "m"}, nil)
		if err == nil || err.Error() != c.agent+": "+why {
			t.Errorf("%s/%s: err = %v, want %q", c.agent, c.p.Name, err, c.agent+": "+why)
		}
	}
}

func captureCatalogWarn(t *testing.T) *strings.Builder {
	t.Helper()
	var warn strings.Builder
	old := CatalogWarn
	CatalogWarn = &warn
	t.Cleanup(func() { CatalogWarn = old })
	return &warn
}





// The copilot fallback keeps the registry's declared Databricks profile order.
func TestOrderedDatabricksKeepsRegistryOrder(t *testing.T) {
	reg, err := providers.Parse([]byte(`{"providers":{"databricks":{
		"prod":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
		"zeta":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
		"dev":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
		"qa":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
		"alpha":{"preset":"databricks","host":"https://h.cloud.databricks.com"}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	reg.Providers["max"] = &providers.Provider{Name: "max", Provider: "anthropic"}
	want := []string{"prod", "zeta", "dev", "qa", "alpha"}
	if got := providers.DatabricksProfiles(reg); !slices.Equal(got, want) {
		t.Errorf("DatabricksProfiles = %v, want %v", got, want)
	}
}

// dbxServing points a Databricks serving row's routes at srv (anthropic at /anthropic, openai/responses at /v1).
func dbxServing(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("TEST_DBX_KEY", "dbx-key")
	body := fmt.Sprintf(`{"providers":{"databricks":{"dbx":{"preset":"databricks","host":"https://h.cloud.databricks.com",
	  "base_url":%q,"routes":{"openai":%q,"responses":%q},"auth":{"type":"key","env":"TEST_DBX_KEY"}}}}}`,
		srv.URL+"/anthropic", srv.URL+"/v1", srv.URL+"/v1")
	vaulttest.Put(t, vault.Overlay, "providers", body)
}
