package redact

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

var (
	sharedMu sync.Mutex
	current  *sharedState
	source   = vaultValues
)

// sharedState is the process matcher; ok means it holds the vault's values, not the shapes-only fallback.
type sharedState struct {
	m  *Matcher
	ok bool
}

// Shared is the process matcher over the vault's values; unreadable leaves shapes only, one stderr line.
func Shared() *Matcher { return loadShared().m }

// SharedWithin is Shared bounded by d; ok is false when the vault values were unreadable or not read in time.
func SharedWithin(d time.Duration) (*Matcher, bool) {
	done := make(chan *sharedState, 1)
	go func() { done <- loadShared() }()
	select {
	case st := <-done:
		return st.m, st.ok
	case <-time.After(d):
		return FromValues(nil), false
	}
}

func loadShared() *sharedState {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if current == nil {
		vals, err := source()
		current = &sharedState{FromValues(vals), err == nil}
		if err != nil {
			fmt.Fprintln(os.Stderr, "claude-code: redact: vault values unavailable; masking credential shapes only")
		}
	}
	return current
}

// Text masks s with the shared matcher; empty text never opens the vault.
func Text(s string) string {
	if s == "" {
		return s
	}
	return Shared().String(s)
}

// Bytes masks b in place with the shared matcher and returns the hit count.
func Bytes(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	return Shared().Mask(b)
}

// ValueBytes masks vault values in one encoded JSON document or line in place, keeping it valid JSON (JSONWriter).
func ValueBytes(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	hits, _ := Shared().scan(b, true, &jsonLexer{}, nil)
	return hits
}

// vaultValues is empty inside a test binary, so a test never opens a real vault.
func vaultValues() ([]string, error) {
	if testing.Testing() {
		return nil, nil
	}
	secrets, err := VaultRecords()
	if err != nil {
		return nil, err
	}
	vals := make([]string, 0, len(secrets))
	for _, v := range secrets {
		vals = append(vals, v)
	}
	return vals, nil
}

// VaultRecords reads the credential records by name, the set Shared masks and doctor --secrets counts.
func VaultRecords() (map[string]string, error) {
	names, err := credentialNames()
	if err != nil || len(names) == 0 {
		return nil, err
	}
	return vault.ReadSecrets(names)
}

// credentialNames omits labeled records (model listings, catalogs): derived caches, not credentials.
func credentialNames() ([]string, error) {
	var names []string
	err := vault.View(func(tx *vault.Tx) error {
		recs, err := tx.Records()
		if err != nil {
			return err
		}
		for name, r := range recs {
			if r.Label == "" {
				names = append(names, name)
			}
		}
		return nil
	})
	return names, err
}

// StubValues makes the shared matcher read vals for this test and rebuilds it after.
func StubValues(t testing.TB, vals []string) {
	t.Helper()
	StubSource(t, func() ([]string, error) { return vals, nil })
}

// StubSource is StubValues with the read itself stubbed, for a vault that fails or stalls.
func StubSource(t testing.TB, src func() ([]string, error)) {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	prevSource, prev := source, current
	source, current = src, nil
	t.Cleanup(func() {
		sharedMu.Lock()
		defer sharedMu.Unlock()
		source, current = prevSource, prev
	})
}
