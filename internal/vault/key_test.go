package vault

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestMasterKeyRecoversPerKidAfterCommandFailure(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "recoverable-value")
	wantKid := phase1bMeta(t, "kid")
	acct := account(currentVaultID(t))
	ring.mu.Lock()
	delete(ring.values, acct)
	ring.mu.Unlock()
	if err := Update(func(tx *Tx) error {
		return tx.Put(Meta, "key_command", `["/bin/true"]`)
	}); err != nil {
		t.Fatal(err)
	}
	invalidateKey()
	key, source, err := MasterKey()
	if err != nil {
		t.Fatalf("MasterKey with missing pointer and empty command output: %v", err)
	}
	if key == nil || source != "keyring" || key.Kid != wantKid {
		t.Fatalf("MasterKey = %v, %q; want per-kid key %q from keyring", key, source, wantKid)
	}
	checkPointerThenPerKid(t, ring, acct, wantKid)
	phase1bRead(t, "TOKEN", "recoverable-value")
}

func TestRotateKeyForceAcceptsCurrentEnvKey(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "survives-forced-rotation")
	acct := account(currentVaultID(t))
	t.Setenv("HARNESS_VAULT_KEY", ringValue(ring, acct))
	before := phase1bMeta(t, "kid")
	if _, _, err := RotateKey(); err == nil {
		t.Fatal("unforced rotation accepted an env-held key")
	}
	oldKid, newKid, err := RotateKey(ForceRotate())
	if err != nil {
		t.Fatalf("forced rotation with current key in env: %v", err)
	}
	if oldKid != before || newKid == "" || newKid == before {
		t.Fatalf("rotation kids = %q -> %q, want %q -> a new kid", oldKid, newKid, before)
	}
	t.Setenv("HARNESS_VAULT_KEY", ringValue(ring, acct+"."+newKid))
	checkRecordKid(t, "TOKEN", newKid)
	phase1bRead(t, "TOKEN", "survives-forced-rotation")
}

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
	if ring.sets != 0 || len(ring.values) != 0 {
		t.Errorf("EnsureKey mutated keyring after ErrNoAnswer: sets=%d entries=%d", ring.sets, len(ring.values))
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
	nextKey := envFixtureKey(t, newBytes, newKid)
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
	acct := stalePointerRing(t, ring, oldBytes, newBytes, newKid)

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
	checkPointerThenPerKid(t, ring, acct, newKid)
}

// stalePointerRing points the current-key account at oldRoot, stores newRoot under newKid, and clears the env key.
func stalePointerRing(t *testing.T, ring *specKeyring, oldRoot, newRoot []byte, newKid string) string {
	t.Helper()
	id := currentVaultID(t)
	if id == "" {
		t.Fatal("new vault has no vault_id")
	}
	acct := "vault-key/" + id
	ring.values[acct] = base64.StdEncoding.EncodeToString(oldRoot)            // stale pointer
	ring.values[acct+"."+newKid] = base64.StdEncoding.EncodeToString(newRoot) // durable, per-kid
	t.Setenv("HARNESS_VAULT_KEY", "")
	return acct
}

// envFixtureKey resolves root through HARNESS_VAULT_KEY and asserts it came from env with kid.
func envFixtureKey(t *testing.T, root []byte, kid string) *Key {
	t.Helper()
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(root))
	k, source, err := MasterKey()
	if err != nil {
		t.Fatalf("load next key from env for crash fixture: %v", err)
	}
	if k == nil {
		t.Fatal("MasterKey returned nil for the fixture key")
	}
	if source != "env" || k.Kid != kid {
		t.Fatalf("fixture key: source=%q kid=%q, want env and %q", source, k.Kid, kid)
	}
	return k
}

// checkPointerThenPerKid asserts ring saw lookups of both the current-key pointer and kid's per-kid account.
func checkPointerThenPerKid(t *testing.T, ring *specKeyring, acct, kid string) {
	t.Helper()
	ring.mu.Lock()
	defer ring.mu.Unlock()
	seenCurrent, seenPerKid := false, false
	for _, name := range ring.lookups {
		seenCurrent = seenCurrent || name == acct
		seenPerKid = seenPerKid || name == acct+"."+kid
	}
	if !seenCurrent || !seenPerKid {
		t.Errorf("MasterKey must try the pointer then the per-kid account on kid mismatch; lookups=%v", ring.lookups)
	}
}
