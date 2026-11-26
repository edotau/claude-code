package vault

import (
	"bytes"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// TestRecordProbeSkipsWriteWhenSetAtChanged: a `vault set` that lands between the probe's read
// and its write (sinceSet captured before the slow probe ran) must leave the new value's record
// untouched — the probe's verdict was computed against the old value and must not stamp the new one.
func TestRecordProbeSkipsWriteWhenSetAtChanged(t *testing.T) {
	useZeroRootVault(t)
	if err := Store("RACY", "first-value"); err != nil {
		t.Fatal(err)
	}
	staleSetAt := readRecord(t, "RACY").SetAt

	// Simulate a concurrent `vault set` landing while the probe for first-value was in flight.
	if err := Store("RACY", "second-value"); err != nil {
		t.Fatal(err)
	}
	fresh := readRecord(t, "RACY")

	if err := RecordProbe("RACY", "verified", time.Now(), staleSetAt); err != nil {
		t.Fatal(err)
	}
	got := readRecord(t, "RACY")
	if got.VerifyStatus != "" || !got.VerifiedAt.IsZero() {
		t.Fatalf("RecordProbe wrote a stale verdict onto the new value: %+v", got)
	}
	if !reflect.DeepEqual(got, fresh) {
		t.Fatalf("RecordProbe mutated the record despite a SetAt mismatch: got %+v, want %+v", got, fresh)
	}

	// A probe run against the current value (sinceSet matches) must still write.
	if err := RecordProbe("RACY", "verified", time.Now(), fresh.SetAt); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, "RACY"); got.VerifyStatus != "verified" {
		t.Fatalf("RecordProbe with a matching sinceSet did not write: %+v", got)
	}
}

func TestBadRecordSurfacesErrorsWithoutMutation(t *testing.T) {
	useZeroRootVault(t)
	const corrupt = `{"ct":`
	putVault(t, Secrets, "X", corrupt)
	if _, err := LoadMeta(); err == nil {
		t.Fatal("LoadMeta must report a corrupt record")
	}
	if err := RecordProbe("X", "verified", time.Now(), time.Now()); err == nil {
		t.Fatal("RecordProbe must report a corrupt record, not a successful no-op")
	}
	if err := View(func(tx *Tx) error {
		if got, _ := tx.Get(Secrets, "X"); got != corrupt {
			t.Fatalf("corrupt record changed: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMetaRejectsNewerSchema(t *testing.T) {
	useZeroRootVault(t)
	putVault(t, Meta, "schema", strconv.Itoa(currentVaultSchema(t)+1))
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMeta(); err == nil {
		t.Fatal("LoadMeta must reject a newer vault schema")
	}
	if err := RecordProbe("X", "verified", time.Now(), time.Now()); err == nil {
		t.Fatal("RecordProbe must reject a newer vault schema")
	}
	after, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unsupported vault was rewritten")
	}
}
