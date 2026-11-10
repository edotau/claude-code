package vault

import (
	"os"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

type specKeyring struct {
	mu      sync.Mutex
	values  map[string]string
	err     error
	lookups []string
	sets    int
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

func TestMain(m *testing.M) {
	DisableSyncForTests()
	cleanup := hosttest.Setup()
	// Replace every OS-keyring boundary before internal or external tests can run.
	ring := &specKeyring{values: make(map[string]string)}
	keyringLookup, keyringSet = ring.lookup, ring.set
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func useSpecKeyring(t *testing.T, ring *specKeyring) {
	t.Helper()
	lookup, set := keyringLookup, keyringSet
	keyringLookup, keyringSet = ring.lookup, ring.set
	t.Cleanup(func() {
		keyringLookup, keyringSet = lookup, set
	})
}
