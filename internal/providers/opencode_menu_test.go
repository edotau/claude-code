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

func dropFixture(t *testing.T) *Provider {
	t.Helper()
	vaulttest.UseKeyring(t)
	p := &Provider{Name: "dev", Provider: GroupDatabricks, Preset: PresetDatabricks}
	if err := SetOpencodeMenu([]OpencodeModel{{ID: "kimi-k3", Label: "K"}, {ID: "gpt-5", Label: "G5"}, {ID: "gpt-5-mini", Label: "G5m"}, {ID: "glm-5-3", Label: "Z"}}); err != nil {
		t.Fatal(err)
	}
	return p
}

func menuIDs() (ids []string) {
	for _, m := range OpencodeMenu() {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestDropRemovesIDsKeepsOrderAndPreviewWritesNothing(t *testing.T) {
	dropFixture(t)
	if got, err := DropFromMenu([]string{"gpt-5", "databricks-gpt-5-mini", "ghost"}, false); err != nil || !reflect.DeepEqual(got, []string{"gpt-5", "gpt-5-mini"}) {
		t.Fatalf("preview = %v, %v", got, err)
	}
	if got := menuIDs(); len(got) != 4 {
		t.Fatalf("preview wrote the menu: %v", got)
	}
	if _, err := DropFromMenu([]string{"gpt-5", "databricks-gpt-5-mini", "ghost"}, true); err != nil {
		t.Fatal(err)
	}
	if got := menuIDs(); !reflect.DeepEqual(got, []string{"kimi-k3", "glm-5-3"}) {
		t.Fatalf("menu after drop = %v", got)
	}
}
