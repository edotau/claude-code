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

// Status picks the slot the effective identity mints from: a paired SP's M2M secret alone is present for sp and auto only.
func TestResolveOAuthPairedSPSlot(t *testing.T) {
	vaulttest.Use(t)
	for identity, want := range map[string]string{IdentitySP: "vault", IdentityAuto: "vault", IdentityUser: "none"} {
		p := &Provider{Name: "dev", Provider: GroupDatabricks, Identity: identity,
			Auth: Auth{Type: AuthOAuth, SPClientID: "sp-client"}}
		vaulttest.Secret(t, SecretName(p.Provider, p.Name, AuthM2M), "sp-secret")
		if got := Resolve(p); got.Source != want {
			t.Errorf("identity %s: source = %q, want %q", identity, got.Source, want)
		}
		if got, _ := CredSource(p); (got == "missing") != (want == "none") {
			t.Errorf("identity %s: CredSource = %q, disagrees with CredentialPresent %v", identity, got, CredentialPresent(p))
		}
	}
}
