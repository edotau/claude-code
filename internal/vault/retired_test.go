package vault_test

import (
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRetiredAfterTwoRotations(t *testing.T) {
	vaulttest.UseKeyring(t)
	k, err := vault.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	old, next, err := vault.RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	if old != k.Kid {
		t.Fatalf("old = %s, want %s", old, k.Kid)
	}
	second, _, err := vault.RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	if second != next {
		t.Fatalf("second old = %s, want %s", second, next)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if got, want := vault.Retired(tx), []string{k.Kid, next}; !reflect.DeepEqual(got, want) {
			t.Errorf("Retired = %v, want %v", got, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
