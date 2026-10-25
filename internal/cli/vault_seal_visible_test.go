package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

// SealSecret (used by login/mint) and `vault set` (secrets.Store) must land the same visible record.
func TestSealSecretVisibleLikeVaultSet(t *testing.T) {
	useVaultTest(t)
	const val = "abcdefghijklmnopqrstuvwxyz0123"
	if err := secrets.Store("ZZ_STORE_SECRET", val); err != nil {
		t.Fatal(err)
	}
	if err := providers.SealSecret("ZZ_SEALED_SECRET", val); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ZZ_STORE_SECRET", "ZZ_SEALED_SECRET"} {
		if got, src, err := secrets.Lookup(n); err != nil || got != val || src != "vault" {
			t.Errorf("Lookup(%s) = %q (%s) %v", n, got, src, err)
		}
	}
	code, out, _ := captureOutput(t, func() int { return cmdVault([]string{"list"}) })
	if code != 0 || !strings.Contains(out, "ZZ_STORE_SECRET") || !strings.Contains(out, "ZZ_SEALED_SECRET") {
		t.Fatalf("vault list: %d %q", code, out)
	}
	var doc struct {
		Secrets []struct {
			Name, Masked string
			Length       int
		} `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("vault list output not JSON: %v", err)
	}
	got := map[string]string{}
	for _, e := range doc.Secrets {
		got[e.Name] = fmt.Sprintf("%s/%d", e.Masked, e.Length)
	}
	if got["ZZ_STORE_SECRET"] == "" || got["ZZ_STORE_SECRET"] != got["ZZ_SEALED_SECRET"] {
		t.Errorf("vault list renders them differently: %v", got)
	}
}
