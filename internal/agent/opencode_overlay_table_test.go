package agent

import (
	"encoding/json"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestOpencodeOverlayModelsFollowTable checks the opencode overlay's model/small_model are the table's opencode cells,
// with a vault route override reaching only the overridden cell.
func TestOpencodeOverlayModelsFollowTable(t *testing.T) {
	vaulttest.Use(t)
	render := func() map[string]any {
		_, body, _, _, err := OpencodeOverlayRendered()
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	sonnet, _ := tb.Cell("opencode", "sonnet")
	haiku, _ := tb.Cell("opencode", "haiku")
	if doc := render(); doc["model"] != sonnet.Model || doc["small_model"] != haiku.Model {
		t.Errorf("overlay = %v/%v, want the table's %q/%q", doc["model"], doc["small_model"], sonnet.Model, haiku.Model)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"routing":{"cells":{"opencode":{"sonnet":{"model":"github-copilot/overridden-model"}}}}}`)
	if doc := render(); doc["model"] != "github-copilot/overridden-model" || doc["small_model"] != haiku.Model {
		t.Errorf("overridden overlay = %v/%v, want github-copilot/overridden-model and unchanged %q", doc["model"], doc["small_model"], haiku.Model)
	}
}
