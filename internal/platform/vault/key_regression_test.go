package vault

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCrossRootSequentialRotationBothDecrypt reproduces the review's HIGH #1 scenario: two config
// roots share a vault_id (B started as a copy of A's db file). Rotating A then B sequentially must
// not strand either copy's active key — once the per-process cache is cleared (a fresh process),
// both roots must still decrypt their record.
func TestCrossRootSequentialRotationBothDecrypt(t *testing.T) {
	phase1bRing(t)
	rootA := os.Getenv("CLAUDE_CONFIG_DIR")
	phase1bPut(t, "TOKEN", "shared-secret")

	rootB := t.TempDir()
	data, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootB, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "state", "vault.db"), data, 0600); err != nil {
		t.Fatal(err)
	}

	_, k1, err := RotateKey(false) // A: K0 -> K1
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", rootB)
	invalidateKey()
	_, k2, err := RotateKey(false) // B (still showing K0): K0 -> K2, overwriting the shared pointer
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("rotations produced the same kid")
	}

	invalidateKey() // simulate a fresh process: no cached key for either root
	t.Setenv("CLAUDE_CONFIG_DIR", rootB)
	phase1bRead(t, "TOKEN", "shared-secret")

	invalidateKey()
	t.Setenv("CLAUDE_CONFIG_DIR", rootA)
	phase1bRead(t, "TOKEN", "shared-secret")
}

// TestRotateSurvivesCrashBeforePointerPromotion reproduces the review's HIGH #2 scenario directly:
// a rotation's reseal transaction commits (the new kid is current in the db, records resealed
// under it) but the process dies before the convenience current-key pointer is advanced.
// Immutable per-kid storage (R1) means this needs no separate "pending" bookkeeping to recover
// (R2): the stale pointer is irrelevant, because matchKey always finds a key under its own kid
// account regardless of what the pointer says.
func TestRotateSurvivesCrashBeforePointerPromotion(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "committed-before-promotion")
	old, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	vaultID := phase1bMeta(t, "vault_id")
	next, nextValue, err := randomKey(vaultID)
	if err != nil {
		t.Fatal(err)
	}
	acct := "vault-key/" + vaultID
	ring.mu.Lock()
	oldValue := ring.values[acct]
	// Simulate RotateKey's crash window: the new key is already durable under its own immutable
	// kid account, but the pointer (acct) was never advanced past the old key.
	ring.values[acct+"."+next.Kid] = nextValue
	ring.mu.Unlock()
	commitResealed(t, next, "TOKEN", "committed-before-promotion")
	// Before any further rotation even runs, a plain read must already recover via the per-kid
	// account — no "promotion" step is required for K1 to survive.
	invalidateKey()
	phase1bRead(t, "TOKEN", "committed-before-promotion")

	_, gotKid, err := RotateKey(false)
	if err != nil {
		t.Fatal(err)
	}
	current := ringValue(ring, acct)
	retainedOld, retainedNext := ringValue(ring, acct+"."+old.Kid), ringValue(ring, acct+"."+next.Kid)
	if current == "" || gotKid == "" || retainedOld != oldValue || retainedNext != nextValue {
		t.Fatalf("rotation after a crashed promotion lost a key: current=%t kid=%q retainedOld=%t retainedNext=%t",
			current != "", gotKid, retainedOld == oldValue, retainedNext == nextValue)
	}
	if kid := phase1bMeta(t, "kid"); kid != gotKid {
		t.Fatalf("recovery changed committed kid to %q, want %q", kid, gotKid)
	}
	phase1bRead(t, "TOKEN", "committed-before-promotion")
}

// commitResealed commits name sealed under k and advances meta/kid to k, as a rotation's reseal tx does.
func commitResealed(t *testing.T, k *Key, name, value string) {
	t.Helper()
	if err := Update(func(tx *Tx) error {
		if err := tx.PutRecord(name, Record{
			Sealed: k.Seal(name, value), Kid: k.Kid,
		}); err != nil {
			return err
		}
		return tx.Put(Meta, "kid", k.Kid)
	}); err != nil {
		t.Fatal(err)
	}
}
