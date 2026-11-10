package databricks

import (
	"errors"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// A rotation by another process before the row write fails must survive the rollback.
func TestDbxSaveIdentityKeepsConcurrentRotationOnRowFailure(t *testing.T) {
	seedOverlay(t, presetOnly)
	reg := mustLoad(t)
	name := providers.SecretName(providers.GroupDatabricks, "aa", providers.AuthM2M)
	orig := SetRow
	SetRow = func(string, map[string]any) error {
		if err := vault.Store(name, "sp-secret-ROTATED"); err != nil {
			t.Fatal(err)
		}
		return errors.New("overlay write failed")
	}
	t.Cleanup(func() { SetRow = orig })

	if err := SaveIdentity(reg, "aa", providers.IdentitySP, "sp-client-2", "sp-secret-NEW"); err == nil {
		t.Fatal("want the row-write error")
	}
	if got, _, _ := vault.ReadSecret(name); got != "sp-secret-ROTATED" {
		t.Errorf("rollback clobbered the newer secret, stored value length %d", len(got))
	}
}
