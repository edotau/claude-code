package redact

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

var (
	sharedMu sync.Mutex
	current  *sharedState
	source   = vaultValues
)

// sharedState is the process matcher; ok means it holds the vault's values, not the shapes-only fallback.
type sharedState struct {
	m   *Matcher
	ok  bool
	err error
}

// Shared is the process matcher over the vault's values; unreadable leaves shapes only, one stderr line.
func Shared() *Matcher { return loadShared().m }

// SharedWithin is Shared bounded by d; cause says why vault values are missing, announce marks a set or renewed latch.
func SharedWithin(d time.Duration) (m *Matcher, cause string, announce bool) {
	word, age, found := readLatch()
	if found && age >= 0 && age < latchTTL {
		return WithEnvKey(FromValues(nil)), downPhrase(word), false
	}
	st := loadSharedWithin(d)
	if st.ok {
		if found {
			_ = os.Remove(latchPath())
		}
		return st.m, "", false
	}
	word = downWord(st.err)
	return st.m, downPhrase(word), writeLatch(word) == nil
}

func loadSharedWithin(d time.Duration) *sharedState {
	done := make(chan *sharedState, 1)
	go func() { done <- loadShared() }()
	select {
	case st := <-done:
		return st
	case <-time.After(d):
		return &sharedState{WithEnvKey(FromValues(nil)), false, vault.ErrKeyringNoAnswer}
	}
}

func loadShared() *sharedState {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if current == nil {
		vals, err := source()
		current = &sharedState{WithEnvKey(FromValues(vals)), err == nil, err}
		if err != nil {
			fmt.Fprintln(os.Stderr, "claude-code: redact: vault values unavailable; masking credential shapes only")
		}
	}
	return current
}

var (
	latchTTL = time.Minute
	clock    = time.Now
)

var downPhrases = map[string]string{
	"noanswer":    "keyring did not answer",
	"locked":      "no key or vault locked",
	"unavailable": "keyring unavailable",
	"unreadable":  "vault unreadable",
}

func downWord(err error) string {
	switch {
	case errors.Is(err, vault.ErrKeyringNoAnswer):
		return "noanswer"
	case errors.Is(err, vault.ErrLocked):
		return "locked"
	case errors.Is(err, vault.ErrKeyringUnavailable):
		return "unavailable"
	}
	return "unreadable"
}

func downPhrase(word string) string {
	if p, ok := downPhrases[word]; ok {
		return p
	}
	return downPhrases["unreadable"]
}

func latchPath() string { return filepath.Join(paths.StateDir(), "hooks", "secrets-down") }

// readLatch parses "<unix seconds> <cause word>"; a malformed file reads as stale, so it can never skip the vault.
func readLatch() (word string, age time.Duration, found bool) {
	b, err := os.ReadFile(latchPath())
	if err != nil {
		return "", 0, false
	}
	secs, w, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return "", latchTTL, true
	}
	return w, clock().Sub(time.Unix(n, 0)), true
}

func writeLatch(word string) error {
	return paths.AtomicWrite(latchPath(), []byte(strconv.FormatInt(clock().Unix(), 10)+" "+word+"\n"), 0o600)
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

// WithEnvKey makes m mask the env master key as set, trimmed and unpadded, whatever its length or character mix.
func WithEnvKey(m *Matcher) *Matcher {
	if raw := vault.EnvKey(); raw != "" {
		m.AddKey(raw)
	}
	return m
}

// AddKey masks a master key in every form, whichever source (env or keyring) supplied it.
func (m *Matcher) AddKey(raw string) {
	trimmed := strings.TrimSpace(raw)
	unpadded := strings.TrimRight(trimmed, "=")
	m.AddForced("HARNESS_VAULT_KEY", raw, trimmed, unpadded, strings.ReplaceAll(trimmed, "/", `\/`)) // last: PHP-style JSON
	if b, err := base64.StdEncoding.DecodeString(trimmed); err == nil {                              // hex of the key itself is in variants; add decoded-bytes hex
		m.AddForced("HARNESS_VAULT_KEY", hex.EncodeToString(b))
	}
}

// vaultValues reads no vault inside a test binary, so a test never opens a real one.
func vaultValues() ([]string, error) {
	if testing.Testing() {
		return nil, nil
	}
	secrets, err := VaultRecords()
	if err != nil {
		return nil, err
	}
	return slices.Collect(maps.Values(secrets)), nil
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
