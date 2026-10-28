package secrets

import (
	"context"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestEffectiveVaultOnly(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("VAULT_ONLY", "")
	vaulttest.Secret(t, "VAULT_ONLY", "vault-value")
	v, src, shadowed, ok := Effective("VAULT_ONLY")
	if !ok || v != "vault-value" || src != "vault" || shadowed != "" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (vault-value, vault, \"\", true)", v, src, shadowed, ok)
	}
}

func TestEffectiveEnvOverVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "ENV_OVER_VAULT", "vault-value")
	t.Setenv("ENV_OVER_VAULT", "  env-value \n")
	v, src, shadowed, ok := Effective("ENV_OVER_VAULT")
	if !ok || v != "env-value" || src != "env" || shadowed != "vault" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (env-value, env, vault, true)", v, src, shadowed, ok)
	}
	if gv, gsrc, gok := Get("ENV_OVER_VAULT"); !gok || gv != "vault-value" || gsrc != "vault" {
		t.Fatalf("Get = (%q, %q, %v), want (vault-value, vault, true)", gv, gsrc, gok)
	}
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if v, src, sh, ok := Effective("ENV_OVER_VAULT"); !ok || v != "env-value" || src != "env" || sh != "vault" {
		t.Fatalf("Effective must detect shadowed vault without a key: (%q, %q, %q, %v)", v, src, sh, ok)
	}
}

func TestEffectiveEnvOnly(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("ONLY_IN_ENV", "env-value")
	if v, src, sh, ok := Effective("ONLY_IN_ENV"); !ok || v != "env-value" || src != "env" || sh != "" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (env-value, env, \"\", true)", v, src, sh, ok)
	}
}

func TestEffectiveMiss(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("NEVER_SET_XYZ", "")
	v, src, shadowed, ok := Effective("NEVER_SET_XYZ")
	if ok || v != "" || src != "" || shadowed != "" {
		t.Fatalf("Effective(miss) = (%q, %q, %q, %v), want all empty, ok=false", v, src, shadowed, ok)
	}
}

func TestEffectiveLockedVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "LOCKED", "vault-value")
	t.Setenv("LOCKED", "")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if v, src, sh, ok := Effective("LOCKED"); ok || v != "" || src != "" || sh != "" {
		t.Fatalf("Effective(locked) = (%q, %q, %q, %v), want empty, false", v, src, sh, ok)
	}
}

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
	if _, src, _, ok := Effective("CROSS_NAME"); !ok || src != "env" {
		t.Fatalf("Effective source = %q (ok=%v), want env", src, ok)
	}
	if v, src, ok := Get("CROSS_NAME"); !ok || v != "vault-value" || src != "vault" {
		t.Fatalf("Get = (%q, %q, %v), want (vault-value, vault, true)", v, src, ok)
	}
}
