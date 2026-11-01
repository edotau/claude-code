package secretstore

import (
	"testing"

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
