package providers

import (
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestOpencodeMenuPrecedence(t *testing.T) {
	vaulttest.Use(t)
	if got := OpencodeMenu(); !reflect.DeepEqual(got, DatabricksOpencodeMenu) || OpencodeMenuFromVault() {
		t.Fatalf("absent: want seed, from-vault false; got %d entries, fromVault=%v", len(got), OpencodeMenuFromVault())
	}
	custom := []OpencodeModel{{ID: "kimi-k3", Label: "Kimi"}, {ID: "gpt-5-5", Label: "G", Responses: true}}
	if err := SetOpencodeMenu(custom); err != nil {
		t.Fatal(err)
	}
	if got := OpencodeMenu(); !reflect.DeepEqual(got, custom) || !OpencodeMenuFromVault() {
		t.Fatalf("set: got %v fromVault=%v", got, OpencodeMenuFromVault())
	}
	for _, bad := range []string{"{not json", "[]", `{"id":"x"}`} {
		vaulttest.Put(t, vault.Overlay, opencodeMenuKey, bad)
		if got := OpencodeMenu(); !reflect.DeepEqual(got, DatabricksOpencodeMenu) || OpencodeMenuFromVault() {
			t.Errorf("corrupt %q: want seed, got %v", bad, got)
		}
	}
}

func TestSetOpencodeMenuEmptyResets(t *testing.T) {
	vaulttest.Use(t)
	if err := SetOpencodeMenu([]OpencodeModel{{ID: "a", Label: "A"}}); err != nil {
		t.Fatal(err)
	}
	if err := SetOpencodeMenu(nil); err != nil {
		t.Fatal(err)
	}
	if OpencodeMenuFromVault() {
		t.Error("empty menu must delete the key")
	}
	_ = vault.View(func(tx *vault.Tx) error {
		if _, ok := tx.Get(vault.Overlay, opencodeMenuKey); ok {
			t.Error("key still present after reset")
		}
		return nil
	})
}

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
