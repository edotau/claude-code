package agent

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Regression: an explicit GPT model (Astra) must reach codex argv, not be swapped for the first Responses entry (Sol).
func TestCodexKeepsExplicitGPTModel(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "gpt-6-1-sol", Responses: true}, {ID: "gpt-6-astra", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		p     *providers.Provider
		model string
	}{
		{"databricks astra", dbxRow("dev", "databricks-"), "databricks-gpt-6-astra"},
		{"non-databricks astra", &providers.Provider{Name: "other"}, "gpt-6-astra"},
	}
	for _, c := range cases {
		_, argv, err := codex(c.p, "http://127.0.0.1/x", c.model, "", &Headless{Effort: "high"})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := "model=" + TOMLQuote(c.model); !slices.Contains(argv, want) {
			t.Errorf("%s: argv %q lacks %s", c.name, argv, want)
		}
		for _, sol := range []string{"gpt-6-1-sol", "databricks-gpt-6-1-sol"} {
			if slices.Contains(argv, "model="+TOMLQuote(sol)) {
				t.Errorf("%s: argv %q routed to %s", c.name, argv, sol)
			}
		}
		if want := "model_reasoning_effort=" + TOMLQuote("high"); !slices.Contains(argv, want) {
			t.Errorf("%s: argv %q lacks %s", c.name, argv, want)
		}
	}
}
