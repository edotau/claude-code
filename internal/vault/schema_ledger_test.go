package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"

	"github.com/edotau/claude-code/internal/schemas"
)

func ledgerLines(t *testing.T) []map[string]any {
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

// TestVaultSchema2RestampedOnFirstUpdateWithOneLedgerLine pins: schema 2 -> 3 on first Update with one ledger line; a fresh vault logs none.
func TestVaultSchema2RestampedOnFirstUpdateWithOneLedgerLine(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if l := ledgerLines(t); len(l) != 0 {
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
	l := ledgerLines(t)
	if len(l) != 1 || l[0]["format"] != "vault" || l[0]["from"] != float64(2) || l[0]["to"] != float64(3) {
		t.Fatalf("ledger = %v, want one {format:vault from:2 to:3}", l)
	}

	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := len(ledgerLines(t)); n != 1 {
		t.Errorf("a second Update added ledger lines: %d total", n)
	}
}

// TestUpdateErrorLeavesNoLedgerLine pins: a failed Update rolls back the 2->3 stamp and logs nothing; a later success logs one line.
func TestUpdateErrorLeavesNoLedgerLine(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error { return tx.SetSchema(2) }); err != nil {
		t.Fatal(err)
	}
	// SetSchema(2) is itself an Update at the current version; clear anything it logged.
	base := len(ledgerLines(t))

	boom := errors.New("boom")
	if err := Update(func(tx *Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Update err = %v, want boom", err)
	}
	if err := View(func(tx *Tx) error {
		if s, _ := tx.Get(Meta, "schema"); s != strconv.Itoa(2) {
			t.Errorf("schema after failed Update = %q, want 2", s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(ledgerLines(t)); n != base {
		t.Fatalf("failed Update logged %d ledger line(s), want 0", n-base)
	}

	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	l := ledgerLines(t)
	if len(l) != base+1 || l[base]["from"] != float64(2) || l[base]["to"] != float64(3) {
		t.Fatalf("ledger = %v, want exactly one new 2->3 line", l)
	}
}
