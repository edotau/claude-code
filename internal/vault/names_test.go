package vault

import (
	"encoding/base64"
	"path/filepath"
	"slices"
	"testing"
)

// Names must list a bucket's keys in sorted (bbolt byte) order, excluding any nested bucket, and
// return an empty non-nil slice for a nil transaction.
func TestNamesOrderAndNoValues(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	const bucket = "namestest"
	if err := Update(func(tx *Tx) error {
		for _, k := range []string{"b", "a", "B", "a0"} {
			if err := tx.Put(bucket, k, "v"); err != nil {
				return err
			}
		}
		// A nested bucket inside the same top-level bucket must never show up as a "name".
		_, err := tx.tx.Bucket([]byte(bucket)).CreateBucket([]byte("nested"))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := View(func(tx *Tx) error {
		got = tx.Names(bucket)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"B", "a", "a0", "b"}
	if !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}

	var nilTx Tx
	if got := nilTx.Names(bucket); got == nil || len(got) != 0 {
		t.Errorf("Names() on nil Tx = %#v, want empty non-nil slice", got)
	}
}

// SnapshotTo against a path that does not exist must error, not silently produce an empty/partial copy.
func TestSnapshotToMissing(t *testing.T) {
	dir := t.TempDir()
	if err := SnapshotTo(filepath.Join(dir, "does-not-exist.db"), nil); err == nil {
		t.Error("SnapshotTo(missing path) = nil error, want one")
	}
}
