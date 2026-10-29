package vault

import (
	"errors"
	"strconv"
	"testing"
)

// TestUpdateErrorLeavesNoLedgerLineStd11 pins: a failed Update rolls back the 2->3 stamp and logs nothing; a later success logs one line.
func TestUpdateErrorLeavesNoLedgerLineStd11(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error { return tx.SetSchema(2) }); err != nil {
		t.Fatal(err)
	}
	// SetSchema(2) is itself an Update at the current version; clear anything it logged.
	base := len(ledgerLinesStd5(t))

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
	if n := len(ledgerLinesStd5(t)); n != base {
		t.Fatalf("failed Update logged %d ledger line(s), want 0", n-base)
	}

	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	l := ledgerLinesStd5(t)
	if len(l) != base+1 || l[base]["from"] != float64(2) || l[base]["to"] != float64(3) {
		t.Fatalf("ledger = %v, want exactly one new 2->3 line", l)
	}
}
