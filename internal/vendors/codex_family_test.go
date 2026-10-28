package vendors

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Regression (decision 24e7c144): a chat-only gpt-* model (gpt-oss) is never posted to /responses; codex swaps it.
func TestCodexChatOnlyGPTSwapped(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "gpt-oss-120b"}, {ID: "gpt-6-1-sol", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{
		"databricks-gpt-oss-120b": "databricks-gpt-6-1-sol",
		"databricks-gpt-6-1-sol":  "databricks-gpt-6-1-sol",
	} {
		_, argv, err := codex(dbxRow("dev", "databricks-"), "http://127.0.0.1/x", model, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if w := "model=" + tomlStr(want); !slices.Contains(argv, w) {
			t.Errorf("%s: argv %q lacks %s", model, argv, w)
		}
	}
}
