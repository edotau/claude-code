package providers

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCustomEnvRoundTripAndSecretRefs(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Env, "SEEDED", `raw "quotes" and \slashes`)
	if got := CustomEnv()["SEEDED"]; got != `raw "quotes" and \slashes` {
		t.Fatalf("vault env read = %q, want raw value", got)
	}
	for k, v := range map[string]string{"PLAIN": "a b#c", "VAULTED": SecretRef, "GONE": SecretRef} {
		if err := SetCustomEnv(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if got := CustomEnv()["PLAIN"]; got != "a b#c" {
		t.Errorf("PLAIN round-trip = %q", got)
	}
	if SecretRef != "@secret" {
		t.Errorf("SecretRef = %q, want @secret", SecretRef)
	}
	vals, missing, err := ResolveCustomEnv(func(k string) (string, bool) { return "secret", k == "VAULTED" })
	if err != nil || vals["VAULTED"] != "secret" || vals["PLAIN"] != "a b#c" || vals["SEEDED"] != `raw "quotes" and \slashes` ||
		!slices.Equal(missing, []string{"GONE"}) {
		t.Errorf("vals=%v missing=%v err=%v", vals, missing, err)
	}
	if _, ok := vals["GONE"]; ok {
		t.Errorf("an unresolved secret ref must not be set: %v", vals)
	}
	if err := SetCustomEnv("PLAIN", ""); err != nil || CustomEnv()["PLAIN"] != "" {
		t.Errorf("delete failed: %v %v", err, CustomEnv())
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if got, ok := tx.Get(vault.Env, "PLAIN"); ok {
			t.Errorf("deleted env key still present: %q", got)
		}
		for key, want := range map[string]string{"SEEDED": `raw "quotes" and \slashes`, "VAULTED": SecretRef} {
			if got, ok := tx.Get(vault.Env, key); !ok || got != want {
				t.Errorf("vault env %s = %q, present %v; want %q", key, got, ok, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// An env bucket that cannot be read surfaces as ResolveCustomEnv's error, not an empty table.
func TestResolveCustomEnvReadError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Dir(vault.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vault.Path(), []byte("not a bbolt file"), 0o600); err != nil {
		t.Fatal(err)
	}
	vals, missing, err := ResolveCustomEnv(func(string) (string, bool) { return "", false })
	if err == nil || len(vals) != 0 || len(missing) != 0 {
		t.Errorf("ResolveCustomEnv(corrupt vault) = %v, %v, %v; want an error and nothing resolved", vals, missing, err)
	}
}
