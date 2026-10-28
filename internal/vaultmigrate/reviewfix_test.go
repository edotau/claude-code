package vaultmigrate

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const patRow = `{"kind":"anthropic","base_url":"https://api.anthropic.com","auth":{"type":"pat"}}`

func TestNestedRetiredProfileNameAndSecretMigrate(t *testing.T) {
	seed(t, `{"providers":{"anthropic":{"subscription":`+patRow+`}}}`, nil)
	if err := vault.UpdateSealed(func(tx *vault.Tx, k *vault.Key) error {
		return tx.SealSecret(k, "PROVIDER_SUBSCRIPTION_PAT", "tok", vault.Record{})
	}); err != nil {
		t.Fatal(err)
	}
	apply(t)
	reg, _ := Current()
	p := reg.Providers["max"]
	if p == nil || p.Auth.Type != providers.AuthPAT {
		t.Fatalf("nested retired subscription not renamed to max: %+v", p)
	}
	if reg.Providers["subscription"] != nil {
		t.Error("retired name subscription survived")
	}
	if v, ok := read(t, providers.AuthSecretName(p)); !ok || v != "tok" {
		t.Errorf("secret %s = %q ok=%v, want tok", providers.AuthSecretName(p), v, ok)
	}
}

func TestPlanStampsSchemaWhenUnstamped(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.Update(func(tx *vault.Tx) error {
		if err := tx.Put(vault.Pins, "HARNESS_PROVIDER", "dev"); err != nil {
			return err
		}
		return tx.Put(vault.Meta, "schema", "")
	}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range plan(t).Changes {
		found = found || c.Kind == KindSchema
	}
	if !found {
		t.Error("no schema step for a vault without meta/schema")
	}
}
