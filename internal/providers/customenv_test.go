package providers

import (
	"slices"
	"testing"
)

func TestCustomEnvRoundTripAndKeyringRefs(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
}
