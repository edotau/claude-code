package launch

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestVaultDoctorChecks(t *testing.T) {
	t.Run("missing vault", func(t *testing.T) {
		hermetic(t, "")
		checks := vaultDoctorChecks()
		c := findCheck(checks, "vault")
		if c == nil || c.Detail != "not created yet" || !c.OK {
			t.Errorf("vault row = %+v", c)
		}
		if len(checks) != 1 {
			t.Errorf("want only the vault row, got %+v", checks)
		}
	})
	t.Run("secrets and retired", func(t *testing.T) {
		hermetic(t, "")
		t.Setenv("HARNESS_VAULT_KEY", "")
		vaulttest.Keyring(t)
		vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
		if _, _, err := vault.RotateKey(); err != nil {
			t.Fatal(err)
		}
		c := findCheck(vaultDoctorChecks(), "vault")
		if c == nil || !c.OK {
			t.Fatalf("vault row = %+v", c)
		}
		for _, want := range []string{vault.Path(), "key keyring", "1 secret(s)", "1 retired"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail %q lacks %q", c.Detail, want)
			}
		}
		if strings.Contains(c.Detail, "sk-test") {
			t.Error("doctor printed a secret")
		}
	})
}
