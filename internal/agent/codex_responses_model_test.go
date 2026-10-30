package agent

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Regression: codex on a Databricks row was handed the opus slot, which Databricks rejects on Responses.
func TestCodexResponsesModel(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "claude-opus-5-5"}, {ID: "gpt-6-1-sol", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		p     *providers.Provider
		model string
		want  string
	}{
		{"databricks claude slot", dbxRow("dev", "databricks-"), "databricks-claude-opus-5-5", "databricks-gpt-6-1-sol"},
		{"databricks gpt kept", dbxRow("dev", "databricks-"), "databricks-gpt-6-1-sol", "databricks-gpt-6-1-sol"},
		{"non-databricks kept", &providers.Provider{Name: "other"}, "claude-opus-5-5", "claude-opus-5-5"},
	}
	for _, c := range cases {
		_, argv, err := codex(c.p, "http://127.0.0.1/x", c.model, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := "model=" + tomlQuote(c.want); !slices.Contains(argv, want) {
			t.Errorf("%s: argv %q lacks %s", c.name, argv, want)
		}
	}
}
