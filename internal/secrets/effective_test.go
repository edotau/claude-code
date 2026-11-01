package secrets

import (
	"context"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCrossCallerAgreeEnvWinsButGetPrefersVault(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Secret(t, "CROSS_NAME", "vault-value")
	t.Setenv("CROSS_NAME", "env-value")
	p := &providers.Provider{
		Name: "x", Protocol: providers.ProtocolAnthropic,
		Auth: providers.Auth{Type: providers.AuthKey, Header: providers.HeaderXAPIKey, Env: "CROSS_NAME"},
	}
	cv, err := providers.Credential(context.Background(), p)
	if err != nil || cv != "env-value" {
		t.Fatalf("providers.Credential = (%q, %v), want (env-value, nil)", cv, err)
	}
	if st := Resolve(p); st.Source != "env" {
		t.Fatalf("Resolve(p).Source = %q, want env", st.Source)
	}
	if _, src, _, ok := secretstore.Effective("CROSS_NAME"); !ok || src != "env" {
		t.Fatalf("Effective source = %q (ok=%v), want env", src, ok)
	}
	if v, src, ok := secretstore.Get("CROSS_NAME"); !ok || v != "vault-value" || src != "vault" {
		t.Fatalf("Get = (%q, %q, %v), want (vault-value, vault, true)", v, src, ok)
	}
}
