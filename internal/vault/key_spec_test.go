package vault

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/keyring"
)

type specKeyring struct {
	mu      sync.Mutex
	values  map[string]string
	err     error
	lookups []string
	sets    int
	deletes int
}

func (s *specKeyring) lookup(name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups = append(s.lookups, name)
	if s.err != nil {
		return "", false, s.err
	}
	value, ok := s.values[name]
	return value, ok, nil
}

func (s *specKeyring) set(name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	s.values[name] = value
	return nil
}

func (s *specKeyring) delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	delete(s.values, name)
	return nil
}

func TestMain(m *testing.M) {
	// Replace every OS-keyring boundary before internal or external tests can run.
	ring := &specKeyring{values: make(map[string]string)}
	keyringLookup, keyringSet, keyringDelete = ring.lookup, ring.set, ring.delete
	os.Exit(m.Run())
}

func useSpecKeyring(t *testing.T, ring *specKeyring) {
	t.Helper()
	lookup, set, del := keyringLookup, keyringSet, keyringDelete
	keyringLookup, keyringSet, keyringDelete = ring.lookup, ring.set, ring.delete
	t.Cleanup(func() {
		keyringLookup, keyringSet, keyringDelete = lookup, set, del
	})
}

func TestEnsureKeyNoAnswerDoesNotGenerate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")
	ring := &specKeyring{
		values: make(map[string]string),
		err:    keyring.ErrNoAnswer,
	}
	useSpecKeyring(t, ring)

	key, err := EnsureKey()
	if err == nil {
		t.Fatal("EnsureKey succeeded after keyring.ErrNoAnswer")
	}
	if key != nil {
		t.Fatal("EnsureKey returned a key after keyring.ErrNoAnswer")
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

func TestMasterKeyRotationResumeFromNext(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	oldBytes := bytes.Repeat([]byte{0x31}, 32)
	newBytes := bytes.Repeat([]byte{0x72}, 32)
	newHash := sha256.Sum256(newBytes)
	newKid := hex.EncodeToString(newHash[:])[:16]
	ring := &specKeyring{values: make(map[string]string)}
	useSpecKeyring(t, ring)

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
	// Crash after the re-seal transaction commits, before vault-key is promoted.
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
	ring.values[acct] = base64.StdEncoding.EncodeToString(oldBytes)
	ring.values[acct+".next"] = base64.StdEncoding.EncodeToString(newBytes)
	t.Setenv("HARNESS_VAULT_KEY", "")

	key, source, err := MasterKey()
	if err != nil {
		t.Fatalf("MasterKey did not resume from vault-key.next: %v", err)
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
	seenCurrent, seenNext := false, false
	for _, name := range ring.lookups {
		seenCurrent = seenCurrent || name == acct
		seenNext = seenNext || name == acct+".next"
	}
	if !seenCurrent || !seenNext {
		t.Errorf("MasterKey must try current key then next on kid mismatch; lookups=%v", ring.lookups)
	}
}
