package router

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestBYOKPoolOverlayOverridesSeedAndSeedIsCloned(t *testing.T) {
	vaulttest.UseKeyring(t)
	got := BYOKPool()
	if !slices.Equal(got, DatabricksBYOKPool) {
		t.Fatalf("unset overlay: pool = %v, want the seed %v", got, DatabricksBYOKPool)
	}
	got[0] = "mutated"
	if DatabricksBYOKPool[0] == "mutated" {
		t.Fatal("BYOKPool aliases the seed slice")
	}
	put := func(raw string) {
		t.Helper()
		if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, byokPoolKey, raw) }); err != nil {
			t.Fatal(err)
		}
	}
	put(`["databricks-a","databricks-b"]`)
	if got := BYOKPool(); !slices.Equal(got, []string{"databricks-a", "databricks-b"}) {
		t.Errorf("overlay pool = %v", got)
	}
	for _, bad := range []string{`not json`, `[]`} {
		put(bad)
		if got := BYOKPool(); !slices.Equal(got, DatabricksBYOKPool) {
			t.Errorf("overlay %q: pool = %v, want the seed", bad, got)
		}
	}
}
