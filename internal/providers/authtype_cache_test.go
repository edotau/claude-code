package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/vault"
)

// A mint drops the old hash-named DATABRICKS_M2M_<HASH>_ACCESS slot so it is not left sealed in the vault.
func TestM2MMintDeletesLegacyHashSlot(t *testing.T) {
	p, secretName := clientRow(t, false)
	legacySwept.Store(false) // a fresh process; an earlier test's mint may have set it
	sum := sha256.Sum256([]byte("h.example.com\x00client\x00secret"))
	legacy := "DATABRICKS_M2M_" + strings.ToUpper(hex.EncodeToString(sum[:8])) + "_ACCESS"
	if err := vault.Store(legacy, "stale"); err != nil {
		t.Fatal(err)
	}
	orig := mintM2M
	mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
		return oauth.Token{Access: jwtExpIn(time.Hour, "")}, nil
	}
	t.Cleanup(func() { mintM2M = orig })
	if _, err := m2mToken(context.Background(), p, secretName, "h.example.com", "client", "secret", true); err != nil {
		t.Fatal(err)
	}
	if vaultHas(t, legacy) {
		t.Fatalf("legacy slot %s survived a mint", legacy)
	}
}

// A mint with no legacy hash slot writes no secret_delete audit entry: the slot is checked before the delete.
func TestM2MMintSkipsDeleteWhenLegacySlotAbsent(t *testing.T) {
	p, secretName := clientRow(t, false)
	orig := mintM2M
	mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
		return oauth.Token{Access: jwtExpIn(time.Hour, "")}, nil
	}
	t.Cleanup(func() { mintM2M = orig })
	since := time.Now().Add(-time.Second)
	if _, err := m2mToken(context.Background(), p, secretName, "h.example.com", "client", "secret", true); err != nil {
		t.Fatal(err)
	}
	events, err := audit.Read(since)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == audit.SecretDelete {
			t.Fatalf("mint audited a delete with no legacy slot present: %+v", e)
		}
	}
}

// The <PROFILE>_M2M_ACCESS cache slot is a route secret, so it is filtered from child env like <PROFILE>_M2M.
func TestRouteSecretCoversM2MAccessSlot(t *testing.T) {
	r := &Registry{}
	if !r.RouteSecret("DATABRICKS_DEV_M2M_ACCESS") {
		t.Fatal("DATABRICKS_DEV_M2M_ACCESS not treated as a route secret")
	}
	if r.RouteSecret("DATABRICKS_DEV_ACCESS") {
		t.Fatal("unrelated _ACCESS name treated as a route secret")
	}
}
