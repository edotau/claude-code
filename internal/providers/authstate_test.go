package providers

import (
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestResolveProfileStoredCredentialWithoutEnv(t *testing.T) {
	vaulttest.Use(t)
	for _, typ := range []string{AuthPAT, AuthOAuth, AuthM2M, AuthKey} {
		t.Run(typ, func(t *testing.T) {
			p := &Provider{Name: "stored-" + typ, Provider: GroupDatabricks, Auth: Auth{Type: typ}}
			name := SecretName(p.Provider, p.Name, typ)
			t.Setenv(name, "")
			if got := Resolve(p); got.Source != "none" {
				t.Fatalf("unseeded source = %q, want none", got.Source)
			}
			vaulttest.Secret(t, name, "profile-credential")
			if got := Resolve(p); got.Source != "vault" || got.Note != "" {
				t.Fatalf("stored profile status = %+v, want vault without a warning", got)
			}
		})
	}
}
