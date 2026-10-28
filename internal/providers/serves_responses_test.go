package providers

import (
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A menu entry's Responses flag wins over the catalog either way; unlisted ids follow the catalog.
func TestServesResponsesMenuOverridesCatalog(t *testing.T) {
	vaulttest.Use(t)
	p := &Provider{Name: "dev", Preset: PresetDatabricks, ModelPrefix: "databricks-"}
	if err := SetOpencodeMenu([]OpencodeModel{{ID: "gpt-6-1-sol", Responses: false}, {ID: "gpt-oss-120b", Responses: true}}); err != nil {
		t.Fatal(err)
	}
	if ServesResponses(p, "databricks-gpt-6-1-sol") || ResponsesID("gpt-6-1-sol") {
		t.Error("menu Responses:false on a catalog Responses-only id must win")
	}
	if !ServesResponses(p, "databricks-gpt-oss-120b") || !ResponsesID("gpt-oss-120b") {
		t.Error("menu Responses:true on gpt-oss-120b must win")
	}
	if !ServesResponses(p, "gpt-5-5") || !ResponsesID("gpt-5-5") {
		t.Error("unlisted catalog Responses-only id must serve Responses")
	}
}
