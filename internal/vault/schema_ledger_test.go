package vault

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/edotau/claude-code/internal/schemas"
)

func ledgerLinesStd5(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(schemas.LedgerPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// TestVaultSchema2RestampedStd5 pins: schema 2 -> 3 on first Update with one ledger line; a fresh vault logs none.
func TestVaultSchema2RestampedStd5(t *testing.T) {
	useZeroRootVault(t)
	found := false
	for _, f := range schemas.All() {
		found = found || f.Name == "vault"
	}
	if !found {
		t.Error(`schemas.All() has no "vault" row`)
	}

	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if l := ledgerLinesStd5(t); len(l) != 0 {
		t.Fatalf("fresh vault wrote ledger lines: %v", l)
	}

	if err := Update(func(tx *Tx) error { return tx.SetSchema(2) }); err != nil {
		t.Fatal(err)
	}
	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := View(func(tx *Tx) error {
		if s, _ := tx.Get(Meta, "schema"); s != strconv.Itoa(3) {
			t.Errorf("schema after Update = %q, want 3", s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	l := ledgerLinesStd5(t)
	if len(l) != 1 || l[0]["format"] != "vault" || l[0]["from"] != float64(2) || l[0]["to"] != float64(3) {
		t.Fatalf("ledger = %v, want one {format:vault from:2 to:3}", l)
	}

	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := len(ledgerLinesStd5(t)); n != 1 {
		t.Errorf("a second Update added ledger lines: %d total", n)
	}
}
