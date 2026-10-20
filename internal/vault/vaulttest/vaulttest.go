package vaulttest

import (
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/vault"
)

func Use(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	return dir
}

func Put(t testing.TB, bucket, key, val string) {
	t.Helper()
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(bucket, key, val) }); err != nil {
		t.Fatal(err)
	}
}

// FakeKeyring is an in-memory stand-in for the OS keyring; Down makes every call look like a
// keyring that could not positively establish absence, Slow makes it look like one that timed out.
type FakeKeyring struct {
	mu     sync.Mutex
	values map[string]string
	Down   bool
	Slow   bool
}

func (f *FakeKeyring) lookup(name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return "", false, keyring.ErrUnavailable
	}
	if f.Slow {
		return "", false, keyring.ErrNoAnswer
	}
	v, ok := f.values[name]
	return v, ok, nil
}

func (f *FakeKeyring) set(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return keyring.ErrUnavailable
	}
	if f.Slow {
		return keyring.ErrNoAnswer
	}
	f.values[name] = value
	return nil
}

func (f *FakeKeyring) delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return keyring.ErrUnavailable
	}
	if f.Slow {
		return keyring.ErrNoAnswer
	}
	delete(f.values, name)
	return nil
}

// Keyring swaps the vault package's keyring seams for an in-memory FakeKeyring, restoring the
// originals on cleanup.
func Keyring(t testing.TB) *FakeKeyring {
	t.Helper()
	f := &FakeKeyring{values: make(map[string]string)}
	t.Cleanup(vault.UseKeyringSeams(f.lookup, f.set, f.delete))
	return f
}

// UseKeyring is Use without a HARNESS_VAULT_KEY override, so the master key resolves through the
// (fake) keyring instead of the env var.
func UseKeyring(t testing.TB) (string, *FakeKeyring) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HARNESS_VAULT_KEY", "")
	return dir, Keyring(t)
}

func Secret(t testing.TB, name, value string) {
	t.Helper()
	k, err := vault.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	r := vault.Record{Sealed: k.Seal(name, value), Kid: k.Kid, Len: len(value), SetAt: time.Now()}
	if err = vault.Update(func(tx *vault.Tx) error { return tx.PutRecord(name, r) }); err != nil {
		t.Fatal(err)
	}
}
