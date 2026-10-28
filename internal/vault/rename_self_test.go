package vault_test

import (
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRenameSecretToSameNameKeepsValue(t *testing.T) {
	vaulttest.UseKeyring(t)
	if err := vault.UpdateSealed(func(tx *vault.Tx, k *vault.Key) error {
		return tx.SealSecret(k, "SELF_NAME", "v1", vault.Record{})
	}); err != nil {
		t.Fatal(err)
	}
	if err := vault.UpdateSealed(func(tx *vault.Tx, k *vault.Key) error { return tx.RenameSecret(k, "SELF_NAME", "SELF_NAME") }); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := vault.ReadSecret("SELF_NAME"); err != nil || !ok || v != "v1" {
		t.Fatalf("after self-rename: %q ok=%v err=%v, want v1", v, ok, err)
	}
}
