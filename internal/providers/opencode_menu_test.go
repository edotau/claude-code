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

// A menu saved with plain booleans still loads; an absent flag now follows the catalog.
func TestOpencodeMenuLegacyBooleans(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, opencodeMenuKey,
		`[{"id":"gpt-6-sol","label":"S"},{"id":"kimi-k3","label":"K","responses":true},{"id":"gpt-oss-120b","label":"O","responses":true}]`)
	want := []OpencodeModel{{ID: "gpt-6-sol", Label: "S", Responses: true}, {ID: "kimi-k3", Label: "K", Responses: true},
		{ID: "gpt-oss-120b", Label: "O"}}
	if got := OpencodeMenu(); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy menu: got %v want %v", got, want)
	}
}

func TestPickerNewSkipsDatabricksLlama3Instruct(t *testing.T) {
	p := &Provider{Name: "dev", Provider: GroupDatabricks, Preset: PresetDatabricks}
	ready := func(id string) ModelInfo { return ModelInfo{ID: id, Ready: "READY", Task: "llm/v1/chat"} }
	live := []ModelInfo{ready("databricks-meta-llama-3-1-8b-instruct"), ready("databricks-meta-llama-3-3-70b-instruct"),
		ready("databricks-new-model")}
	got := pickerNewWithBaseline(p, nil, live, nil)
	if want := []ModelInfo{{ID: "databricks-new-model"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("additions = %+v, want %+v", got, want)
	}
}
