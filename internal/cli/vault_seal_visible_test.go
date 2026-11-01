package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/secretstore"
)

// SealSecret (used by login/mint) and `vault set` (secretstore.Store) must land the same visible record.
func TestSealSecretVisibleLikeVaultSet(t *testing.T) {
	useVaultTest(t)
	const val = "abcdefghijklmnopqrstuvwxyz0123"
	if err := secretstore.Store("ZZ_STORE_SECRET", val); err != nil {
		t.Fatal(err)
	}
	if err := secretstore.Store("ZZ_SEALED_SECRET", val); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ZZ_STORE_SECRET", "ZZ_SEALED_SECRET"} {
		if got, src, err := secretstore.Lookup(n); err != nil || got != val || src != "vault" {
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

// A SealSecret write must carry the same metadata prefix, length and audit entry as `vault set` (secretstore.Store).
func TestSealSecretMetadataAndAuditMatchStore(t *testing.T) {
	useVaultTest(t)
	const val = "dapi-abcdefghijklmnopqrstuvwxyz0123"
	if err := secretstore.Store("ZZ_VIA_STORE", val); err != nil {
		t.Fatal(err)
	}
	if err := secretstore.Store("ZZ_VIA_SEAL", val); err != nil {
		t.Fatal(err)
	}
	stored, err := secrets.ListStored([]string{"ZZ_VIA_STORE", "ZZ_VIA_SEAL"})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]secrets.Stored{}
	for _, s := range stored {
		by[s.Name] = s
	}
	a, b := by["ZZ_VIA_STORE"], by["ZZ_VIA_SEAL"]
	if a.Prefix == "" || a.Prefix != b.Prefix || a.Len != b.Len || a.Len != len(val) {
		t.Errorf("metadata differs: store=%+v seal=%+v", a, b)
	}
	events, err := audit.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, e := range events {
		if e.Action == audit.SecretSet {
			seen[e.Resource] = e.Detail
		}
		if strings.Contains(e.Resource+e.Detail, val) {
			t.Errorf("audit entry carries the value: %+v", e)
		}
	}
	if seen["ZZ_VIA_STORE"] == "" || seen["ZZ_VIA_SEAL"] != seen["ZZ_VIA_STORE"] {
		t.Errorf("audit entries differ: %v", seen)
	}
}
