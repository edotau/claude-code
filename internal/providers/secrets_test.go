package providers

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// The <PROFILE>_M2M_ACCESS cache slot is a route secret, so it is filtered from child env like <PROFILE>_M2M.
func TestRouteSecretCoversM2MAccessSlot(t *testing.T) {
	r := &Registry{}
	if !r.routeSecret("DATABRICKS_DEV_M2M_ACCESS") {
		t.Fatal("DATABRICKS_DEV_M2M_ACCESS not treated as a route secret")
	}
	if r.routeSecret("DATABRICKS_DEV_ACCESS") {
		t.Fatal("unrelated _ACCESS name treated as a route secret")
	}
}

const seededEnv = `raw "quotes" and \slashes`

func TestCustomEnvRoundTripAndSecretRefs(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Env, "SEEDED", seededEnv)
	if got := CustomEnv()["SEEDED"]; got != seededEnv {
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
	t.Run("resolve", resolveCustomEnvRefs)
	t.Run("delete", deleteCustomEnvKey)
}

func resolveCustomEnvRefs(t *testing.T) {
	vals, missing, err := ResolveCustomEnv(func(k string) (string, bool) { return "secret", k == "VAULTED" })
	if err != nil || vals["VAULTED"] != "secret" || vals["PLAIN"] != "a b#c" || vals["SEEDED"] != seededEnv ||
		!slices.Equal(missing, []string{"GONE"}) {
		t.Errorf("vals=%v missing=%v err=%v", vals, missing, err)
	}
	if _, ok := vals["GONE"]; ok {
		t.Errorf("an unresolved secret ref must not be set: %v", vals)
	}
}

func deleteCustomEnvKey(t *testing.T) {
	if err := SetCustomEnv("PLAIN", ""); err != nil || CustomEnv()["PLAIN"] != "" {
		t.Errorf("delete failed: %v %v", err, CustomEnv())
	}
	if err := vault.View(func(tx *vault.Tx) error {
		checkEnvBucketAfterDelete(t, tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func checkEnvBucketAfterDelete(t *testing.T, tx *vault.Tx) {
	t.Helper()
	if got, ok := tx.Get(vault.Env, "PLAIN"); ok {
		t.Errorf("deleted env key still present: %q", got)
	}
	for key, want := range map[string]string{"SEEDED": seededEnv, "VAULTED": SecretRef} {
		if got, ok := tx.Get(vault.Env, key); !ok || got != want {
			t.Errorf("vault env %s = %q, present %v; want %q", key, got, ok, want)
		}
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

func TestSetCustomEnvRefusesStoredDatabricksRoute(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := SetCustomEnv("HARNESS_ROUTE", "databricks:dev")
	if err == nil || !strings.Contains(err.Error(), "HARNESS_ROUTE") || !strings.Contains(err.Error(), "launch flag") {
		t.Fatalf("err = %v, want a refusal naming HARNESS_ROUTE and the flag", err)
	}
	if _, ok := CustomEnv()["HARNESS_ROUTE"]; ok {
		t.Error("the refused route was stored")
	}
	if err := SetCustomEnv("HARNESS_ROUTE", "first-party"); err != nil {
		t.Errorf("a non-databricks route is refused: %v", err)
	}
}
