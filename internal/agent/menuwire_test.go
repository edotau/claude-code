package agent

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestMenuWire(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	cases := []struct{ id, want string }{
		{"gpt-6-1-sol", "databricks-gpt-6-1-sol"},
		{"glm-5-3", "databricks-glm-5-3"},                         // a version-looking id is never rewritten
		{"databricks-glm-5-3", "databricks-glm-5-3"},              // already wire: no double prefix
		{"new-live-only-model", "databricks-new-live-only-model"}, // outside the catalog still takes the row's prefix
	}
	for _, c := range cases {
		if got := menuWire(p, c.id); got != c.want {
			t.Errorf("menuWire(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestCodexSlug(t *testing.T) {
	for id, want := range map[string]string{"gpt-6-1-sol": "gpt-6.1-sol", "gpt-6-luna": "gpt-6-luna", "glm-5-3": "glm-5.3", "unknown-9-9": "unknown-9-9"} {
		if got := codexSlug(id); got != want {
			t.Errorf("codexSlug(%q) = %q, want %q", id, got, want)
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
