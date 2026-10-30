package vendors

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Regression: ask reported the requested model although codex's Responses swap ran another one.
func TestCodexRanModelNamesTheSwappedModel(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "gpt-6-1-sol", Responses: true}, {ID: "gpt-6-astra", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	p := dbxRow("dev", "databricks-")
	for _, c := range []struct{ name, model, want string }{
		{"non-Responses model is swapped", "databricks-claude-opus-x", "databricks-gpt-6-1-sol"},
		{"Responses model is kept", "databricks-gpt-6-astra", "databricks-gpt-6-astra"},
	} {
		if got := CodexRanModel(providers.Target{Provider: p, Model: c.model}); got != c.want {
			t.Errorf("%s: CodexRanModel(%q) = %q, want %q", c.name, c.model, got, c.want)
		}
	}
	if got := CodexRanModel(providers.Target{Model: "m"}); got != "m" {
		t.Errorf("nil provider: got %q, want m", got)
	}
}
