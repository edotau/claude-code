package secrets

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func readRecord(t *testing.T, name string) vault.Record {
	t.Helper()
	var record vault.Record
	if err := vault.View(func(tx *vault.Tx) error {
		var found bool
		var err error
		record, found, err = tx.Record(name)
		if err == nil && !found {
			t.Fatalf("record %s missing", name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestLoadMetaKeylessAndCurrent(t *testing.T) {
	vaulttest.Use(t)
	if err := Store("STORED", "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	r := readRecord(t, "STORED")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("ONLY_IN_ENV", "not-stored")
	for range 2 {
		meta, err := LoadMeta()
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]Meta{"STORED": {
			Prefix: r.Prefix, Len: r.Len, SetAt: r.SetAt,
			VerifiedAt: r.VerifiedAt, VerifyStatus: r.VerifyStatus,
		}}
		if !reflect.DeepEqual(meta, want) {
			t.Fatalf("LoadMeta = %+v, want %+v", meta, want)
		}
	}
	if err := Delete("STORED"); err != nil {
		t.Fatal(err)
	}
	if meta, err := LoadMeta(); err != nil || len(meta) != 0 {
		t.Fatalf("LoadMeta after Delete = %+v, %v", meta, err)
	}
}

func TestLoadMetaMissingVaultDoesNotCreateIt(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if meta, err := LoadMeta(); err != nil || len(meta) != 0 {
		t.Fatalf("LoadMeta = %+v, %v, want empty, nil", meta, err)
	}
	if _, err := os.Stat(vault.Path()); !os.IsNotExist(err) {
		t.Fatalf("LoadMeta created a vault: %v", err)
	}
}

func TestRecordProbeKeylessPreservesRecord(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "PROBE", "sk-test-abcdefghijklmnopqrstuvwxyz")
	before := readRecord(t, "PROBE")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := RecordProbe("PROBE", "verified", at, before.SetAt); err != nil {
		t.Fatal(err)
	}
	want := before
	want.VerifyStatus, want.VerifiedAt = "verified", at
	if got := readRecord(t, "PROBE"); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecordProbe changed fields beyond the probe: got %+v, want %+v", got, want)
	}
	if meta, err := LoadMeta(); err != nil || meta["PROBE"].VerifyStatus != "verified" || !meta["PROBE"].VerifiedAt.Equal(at) {
		t.Fatalf("LoadMeta probe = %+v, %v", meta, err)
	}
	if err := RecordProbe("MISSING", "rejected", at, at); err != nil {
		t.Fatal(err)
	}
	if got := Names(); len(got) != 1 || got[0] != "PROBE" {
		t.Fatalf("RecordProbe created an absent record: %v", got)
	}
}

// TestRecordProbeSkipsWriteWhenSetAtChanged: a `vault set` that lands between the probe's read
// and its write (sinceSet captured before the slow probe ran) must leave the new value's record
// untouched — the probe's verdict was computed against the old value and must not stamp the new one.
func TestRecordProbeSkipsWriteWhenSetAtChanged(t *testing.T) {
	vaulttest.Use(t)
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

func TestStoreNeverLeaksRawBytesIntoMetadataOrVault(t *testing.T) {
	vaulttest.Use(t)
	values := []string{"aAbBcCdDeEfFgGhHiIjJkKlLmMnNoOpPqQrRsStT12", "zZyYxXwWvVuUtTsSrRqQpPoOnNmMlLkKjJiIhHgG98"}
	for i, name := range []string{"FIRST", "SECOND"} {
		if err := Store(name, values[i]); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	rawVault, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if bytes.Contains(rawMeta, []byte(value)) || bytes.Contains(rawVault, []byte(value)) {
			t.Fatal("metadata or vault contains a plaintext secret")
		}
	}
	fi, err := os.Stat(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("vault mode = %04o, want 0600", fi.Mode().Perm())
	}
}

func TestBadRecordSurfacesErrorsWithoutMutation(t *testing.T) {
	vaulttest.Use(t)
	const corrupt = `{"ct":`
	vaulttest.Put(t, vault.Secrets, "X", corrupt)
	if _, err := LoadMeta(); err == nil {
		t.Fatal("LoadMeta must report a corrupt record")
	}
	if err := RecordProbe("X", "verified", time.Now(), time.Now()); err == nil {
		t.Fatal("RecordProbe must report a corrupt record, not a successful no-op")
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if got, _ := tx.Get(vault.Secrets, "X"); got != corrupt {
			t.Fatalf("corrupt record changed: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMetaRejectsNewerSchema(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", "3")
	before, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMeta(); err == nil {
		t.Fatal("LoadMeta must reject a newer vault schema")
	}
	if err := RecordProbe("X", "verified", time.Now(), time.Now()); err == nil {
		t.Fatal("RecordProbe must reject a newer vault schema")
	}
	after, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unsupported vault was rewritten")
	}
}

func TestStoreClearsVerifyStatusAndDeleteRemovesRow(t *testing.T) {
	vaulttest.Use(t)
	if err := Store("VERIFY_ME", "v1"); err != nil {
		t.Fatal(err)
	}
	setAt := readRecord(t, "VERIFY_ME").SetAt
	at := time.Now()
	if err := RecordProbe("VERIFY_ME", "verified", at, setAt); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if got := m["VERIFY_ME"]; got.VerifyStatus != "verified" || !got.VerifiedAt.Equal(at) {
		t.Fatalf("probe metadata = %+v", got)
	}
	if err := Store("VERIFY_ME", "v2"); err != nil {
		t.Fatal(err)
	}
	m, err = LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if got := m["VERIFY_ME"]; got.VerifyStatus != "" || !got.VerifiedAt.IsZero() {
		t.Fatalf("Store must clear VerifyStatus/VerifiedAt, got %+v", got)
	}
	if err := Delete("VERIFY_ME"); err != nil {
		t.Fatal(err)
	}
	m, err = LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["VERIFY_ME"]; ok {
		t.Fatal("Delete must remove the row entirely")
	}
}

func TestMetaPrefixNeverPersistsWholeValue(t *testing.T) {
	vaulttest.Use(t)
	for _, v := range []string{"abc-def-", "ab-cd-ef-gh-", "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"} {
		if err := Store("SHORT_SEG", v); err != nil {
			t.Fatal(err)
		}
		m, err := LoadMeta()
		if err != nil {
			t.Fatal(err)
		}
		if p := m["SHORT_SEG"].Prefix; len(p) > len(v)/3 {
			t.Fatalf("prefix %q persisted for %q: over a third of the value", p, v)
		}
	}
	if p := MetaPrefix("sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"); p != "sk-ant-oat01-" {
		t.Fatalf("MetaPrefix dropped a genuine vendor prefix: %q", p)
	}
}
