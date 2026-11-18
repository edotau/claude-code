package redact

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

var (
	sharedMu sync.Mutex
	shared   *Matcher
	source   = vaultValues
)

// Shared is the process matcher over the vault's values; unreadable leaves shapes only, one stderr line.
func Shared() *Matcher {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared == nil {
		m, err := Load(source)
		if err != nil {
			fmt.Fprintln(os.Stderr, "claude-code: redact: vault values unavailable; masking credential shapes only")
			m = FromValues(nil)
		}
		shared = m
	}
	return shared
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

// ValueBytes is Bytes without the shape pass, for encoded JSON lines.
func ValueBytes(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	return Shared().MaskValues(b)
}

// vaultValues is empty inside a test binary, so a test never opens a real vault.
func vaultValues() ([]string, error) {
	if testing.Testing() {
		return nil, nil
	}
	names, err := credentialNames()
	if err != nil || len(names) == 0 {
		return nil, err
	}
	secrets, err := vault.ReadSecrets(names)
	if err != nil {
		return nil, err
	}
	vals := make([]string, 0, len(secrets))
	for _, v := range secrets {
		vals = append(vals, v)
	}
	return vals, nil
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
	sharedMu.Lock()
	defer sharedMu.Unlock()
	prevSource, prevShared := source, shared
	source, shared = func() ([]string, error) { return vals, nil }, nil
	t.Cleanup(func() {
		sharedMu.Lock()
		defer sharedMu.Unlock()
		source, shared = prevSource, prevShared
	})
}
