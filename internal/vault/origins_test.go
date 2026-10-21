package vault

import (
	"testing"
)

// TestMemoryOriginsBucketIsPlaintextAndNeedsNoMasterKey: the memory_origins bucket is read/written
// through the ordinary (unsealed) Put/Get path — a caller with no master key at all must succeed.
func TestMemoryOriginsBucketIsPlaintextAndNeedsNoMasterKey(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")

	if err := Update(func(tx *Tx) error { return tx.Put(MemoryOrigins, "repo-a", `{"version":1}`) }); err != nil {
		t.Fatalf("Update(memory_origins) without a master key: %v", err)
	}
	var got string
	var ok bool
	if err := View(func(tx *Tx) error { got, ok = tx.Get(MemoryOrigins, "repo-a"); return nil }); err != nil {
		t.Fatalf("View(memory_origins) without a master key: %v", err)
	}
	if !ok || got != `{"version":1}` {
		t.Errorf("Get(memory_origins, repo-a) = %q, %v, want the stored row", got, ok)
	}
}

// TestMemoryOriginsPutEmptyDeletesRow: Put's generic empty-value-deletes rule applies to this bucket
// the same as any other — the row disappears from both Get and All/Names.
func TestMemoryOriginsPutEmptyDeletesRow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")

	if err := Update(func(tx *Tx) error { return tx.Put(MemoryOrigins, "repo-b", `{"version":1}`) }); err != nil {
		t.Fatal(err)
	}
	if err := Update(func(tx *Tx) error { return tx.Put(MemoryOrigins, "repo-b", "") }); err != nil {
		t.Fatal(err)
	}
	if err := View(func(tx *Tx) error {
		if _, ok := tx.Get(MemoryOrigins, "repo-b"); ok {
			t.Error("row survived an empty-value Put")
		}
		if names := tx.Names(MemoryOrigins); len(names) != 0 {
			t.Errorf("Names(memory_origins) = %v, want empty", names)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMemoryOriginsAllReadsEveryRowInOneTransaction: All returns every stored row in a single View,
// the batched read resolveOrigins relies on to resolve a whole store in one vault round trip.
func TestMemoryOriginsAllReadsEveryRowInOneTransaction(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")

	want := map[string]string{"alpha": `{"key":"alpha"}`, "beta": `{"key":"beta"}`}
	if err := Update(func(tx *Tx) error {
		for k, v := range want {
			if err := tx.Put(MemoryOrigins, k, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := View(func(tx *Tx) error { got = tx.All(MemoryOrigins); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || got["alpha"] != want["alpha"] || got["beta"] != want["beta"] {
		t.Errorf("All(memory_origins) = %v, want %v", got, want)
	}
}
