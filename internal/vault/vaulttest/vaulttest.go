// Package vaulttest gives tests a hermetic vault: a temp config dir, a fixed key, seeded records and a fake keyring.
package vaulttest

import (
	"encoding/base64"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

func Use(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("CLAUDE_PROFILE", "") // the launching session's marker must not steer a hermetic vault
	return dir
}

func Put(t testing.TB, bucket, key, val string) {
	t.Helper()
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(bucket, key, val) }); err != nil {
		t.Fatal(err)
	}
}

// SetRecord sets one field of agent's record (profile, model, effort, context, account or slots.<tier>); "" clears it.
func SetRecord(t testing.TB, agent, field, val string) {
	t.Helper()
	err := vault.Update(func(tx *vault.Tx) error {
		recs, err := tx.AgentRecords()
		if err != nil {
			return err
		}
		r := recs[agent]
		switch tier, isSlot := strings.CutPrefix(field, "slots."); {
		case isSlot:
			slots := maps.Clone(r.Slots)
			if slots == nil {
				slots = map[string]string{}
			}
			if slots[tier] = val; val == "" {
				delete(slots, tier)
			}
			r.Slots = slots
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
			t.Fatalf("vaulttest.SetRecord: unknown field %q", field)
		}
		recs[agent] = r
		return tx.PutAgentRecords(recs)
	})
	if err != nil {
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

func (f *FakeKeyring) delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return vault.ErrKeyringUnavailable
	}
	if f.Slow {
		return vault.ErrKeyringNoAnswer
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
