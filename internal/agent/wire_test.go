package agent

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
