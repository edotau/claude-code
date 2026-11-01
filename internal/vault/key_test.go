package vault

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestEnsureKeyNoAnswerDoesNotGenerate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")
	ring := &specKeyring{
		values: make(map[string]string),
		err:    ErrKeyringNoAnswer,
	}
	useSpecKeyring(t, ring)

	key, err := EnsureKey()
	if err == nil {
		t.Fatal("EnsureKey succeeded after ErrKeyringNoAnswer")
	}
	if key != nil {
		t.Fatal("EnsureKey returned a key after ErrKeyringNoAnswer")
	}
	ring.mu.Lock()
	defer ring.mu.Unlock()
	if len(ring.lookups) == 0 {
		t.Error("EnsureKey did not consult the unavailable keyring")
	}
	if ring.sets != 0 || ring.deletes != 0 || len(ring.values) != 0 {
		t.Errorf("EnsureKey mutated keyring after ErrNoAnswer: sets=%d deletes=%d entries=%d",
			ring.sets, ring.deletes, len(ring.values))
	}
	if err := View(func(tx *Tx) error {
		if kid, ok := tx.Get(Meta, "kid"); ok {
			t.Errorf("EnsureKey persisted kid %q after ErrNoAnswer", kid)
		}
		return nil
	}); err != nil {
		t.Fatalf("read metadata after failed EnsureKey: %v", err)
	}
}

// TestMasterKeyResolvesViaPerKidAccountOnPointerMismatch covers R1/R2's replacement for the
// retired ".next" pending-key scheme: when the convenience current-key pointer disagrees with the
// db's recorded kid (a crashed or cross-root rotation), MasterKey must still resolve the exact key
// via its own immutable per-kid account — durable storage, not a disposable pending slot.
func TestMasterKeyResolvesViaPerKidAccountOnPointerMismatch(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	oldBytes := bytes.Repeat([]byte{0x31}, 32)
	newBytes := bytes.Repeat([]byte{0x72}, 32)
	newHash := sha256.Sum256(newBytes)
	newKid := hex.EncodeToString(newHash[:])[:16]
	ring := &specKeyring{values: make(map[string]string)}
	useSpecKeyring(t, ring)

	// Materialize the vault (and its real vault_id) before deriving the fixture key: the HKDF
	// subkey is salted by vault_id, so deriving one against a not-yet-created vault would mismatch.
	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(newBytes))
	nextKey, source, err := MasterKey()
	if err != nil {
		t.Fatalf("load next key from env for crash fixture: %v", err)
	}
	if nextKey == nil {
		t.Fatal("MasterKey returned nil for the fixture key")
	}
	if source != "env" || nextKey.Kid != newKid {
		t.Fatalf("fixture key: source=%q kid=%q, want env and %q", source, nextKey.Kid, newKid)
	}
	record := Record{Sealed: nextKey.Seal("ROTATED_TOKEN", "rotation-resume-value"), Kid: newKid}
	// Crash after the re-seal transaction commits, before the convenience pointer is advanced.
	if err := Update(func(tx *Tx) error {
		if err := tx.Put(Meta, "kid", newKid); err != nil {
			return err
		}
		return tx.PutRecord("ROTATED_TOKEN", record)
	}); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := View(func(tx *Tx) error {
		id, _ = tx.Get(Meta, "vault_id")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("new vault has no vault_id")
	}
	acct := "vault-key/" + id
	ring.values[acct] = base64.StdEncoding.EncodeToString(oldBytes)            // stale pointer
	ring.values[acct+"."+newKid] = base64.StdEncoding.EncodeToString(newBytes) // durable, per-kid
	t.Setenv("HARNESS_VAULT_KEY", "")

	key, source, err := MasterKey()
	if err != nil {
		t.Fatalf("MasterKey did not resolve the new kid via its immutable account: %v", err)
	}
	if key == nil {
		t.Fatal("MasterKey returned nil without an error")
	}
	if source != "keyring" || key.Kid != newKid {
		t.Errorf("resumed key: source=%q kid=%q, want keyring and %q", source, key.Kid, newKid)
	}
	got, err := key.Open("ROTATED_TOKEN", record)
	if err != nil || got != "rotation-resume-value" {
		t.Errorf("resumed key cannot decrypt committed record: got=%q err=%v", got, err)
	}
	ring.mu.Lock()
	defer ring.mu.Unlock()
	seenCurrent, seenPerKid := false, false
	for _, name := range ring.lookups {
		seenCurrent = seenCurrent || name == acct
		seenPerKid = seenPerKid || name == acct+"."+newKid
	}
	if !seenCurrent || !seenPerKid {
		t.Errorf("MasterKey must try the pointer then the per-kid account on kid mismatch; lookups=%v", ring.lookups)
	}
}
