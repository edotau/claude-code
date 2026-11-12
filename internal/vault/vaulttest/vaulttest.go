// Package vaulttest gives tests a hermetic vault: a temp config dir, a fixed key, seeded records and a fake keyring.
package vaulttest

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

func Use(t testing.TB) string {
	t.Helper()
	vault.DisableSyncForTests()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("CLAUDE_PROFILE", "") // the launching session's marker must not steer a hermetic vault
	return dir
}

// UseFreshKey is Use with a random master key instead of the fixed zero key.
func UseFreshKey(t testing.TB) string {
	t.Helper()
	dir := Use(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(key))
	return dir
}

func Put(t testing.TB, bucket, key, val string) {
	t.Helper()
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(bucket, key, val) }); err != nil {
		t.Fatal(err)
	}
}

// SetRecord sets one field of agent's record (profile, model, effort, context, account, aliases.<alias>); "" clears it.
func SetRecord(t testing.TB, agent, field, val string) {
	t.Helper()
	err := vault.Update(func(tx *vault.Tx) error {
		recs, err := tx.AgentRecords()
		if err != nil {
			return err
		}
		r, ok := setRecordField(recs[agent], field, val)
		if !ok {
			t.Fatalf("vaulttest.SetRecord: unknown field %q", field)
		}
		recs[agent] = r
		return tx.PutAgentRecords(recs)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// setRecordField returns r with field set to val ("" clears it); ok is false for an unknown field.
func setRecordField(r vault.AgentRecord, field, val string) (vault.AgentRecord, bool) {
	switch alias, isAlias := strings.CutPrefix(field, "aliases."); {
	case isAlias:
		r = r.WithAlias(alias, val)
	case field == "profile":
		r.Profile = val
	case field == "model":
		r.Model = val
	case field == "effort":
		r.Effort = val
	case field == "context":
		r.Context = val
	case field == "account":
		r.Account = val
	default:
		return r, false
	}
	return r, true
}

// FakeKeyring is an in-memory OS keyring; Down makes every call unavailable, Slow makes it time out.
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
		return "", false, vault.ErrKeyringUnavailable
	}
	if f.Slow {
		return "", false, vault.ErrKeyringNoAnswer
	}
	v, ok := f.values[name]
	return v, ok, nil
}

func (f *FakeKeyring) set(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return vault.ErrKeyringUnavailable
	}
	if f.Slow {
		return vault.ErrKeyringNoAnswer
	}
	f.values[name] = value
	return nil
}

// Keyring swaps the vault package's keyring seams for a FakeKeyring until cleanup.
func Keyring(t testing.TB) *FakeKeyring {
	t.Helper()
	f := &FakeKeyring{values: make(map[string]string)}
	t.Cleanup(vault.UseKeyringSeams(f.lookup, f.set))
	return f
}

// UseKeyring is Use without HARNESS_VAULT_KEY, so the master key resolves through the fake keyring.
func UseKeyring(t testing.TB) (string, *FakeKeyring) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HARNESS_VAULT_KEY", "")
	t.Setenv("CLAUDE_PROFILE", "") // the launching session's marker must not steer a hermetic vault
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
