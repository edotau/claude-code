package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

// A SealSecret write must carry the same metadata prefix, length and audit entry as `vault set` (secrets.Store).
func TestSealSecretMetadataAndAuditMatchStore(t *testing.T) {
	useVaultTest(t)
	const val = "dapi-abcdefghijklmnopqrstuvwxyz0123"
	if err := secrets.Store("ZZ_VIA_STORE", val); err != nil {
		t.Fatal(err)
	}
	if err := providers.SealSecret("ZZ_VIA_SEAL", val); err != nil {
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
