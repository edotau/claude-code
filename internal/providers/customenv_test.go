package providers

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCustomEnvRoundTripAndKeyringRefs(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Env, "SEEDED", `raw "quotes" and \slashes`)
	if got := CustomEnv()["SEEDED"]; got != `raw "quotes" and \slashes` {
		t.Fatalf("vault env read = %q, want raw value", got)
	}
	for k, v := range map[string]string{"PLAIN": "a b#c", "VAULTED": KeyringRef, "GONE": KeyringRef} {
		if err := SetCustomEnv(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if got := CustomEnv()["PLAIN"]; got != "a b#c" {
		t.Errorf("PLAIN round-trip = %q", got)
	}
	vals, missing := ResolveCustomEnv(func(k string) (string, bool) { return "secret", k == "VAULTED" })
	if vals["VAULTED"] != "secret" || vals["PLAIN"] != "a b#c" || !slices.Equal(missing, []string{"GONE"}) {
		t.Errorf("vals=%v missing=%v", vals, missing)
	}
	if err := SetCustomEnv("PLAIN", ""); err != nil || CustomEnv()["PLAIN"] != "" {
		t.Errorf("delete failed: %v %v", err, CustomEnv())
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if got, ok := tx.Get(vault.Env, "PLAIN"); ok {
			t.Errorf("deleted env key still present: %q", got)
		}
		for key, want := range map[string]string{"SEEDED": `raw "quotes" and \slashes`, "VAULTED": KeyringRef} {
			if got, ok := tx.Get(vault.Env, key); !ok || got != want {
				t.Errorf("vault env %s = %q, present %v; want %q", key, got, ok, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
