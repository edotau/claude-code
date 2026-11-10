package vault

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

// fakeKeyringTool runs the keyring plane as goos with a /bin/sh stub of that OS's tool as the only binary on PATH.
func fakeKeyringTool(t *testing.T, goos, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub keyring tool is a POSIX shell script")
	}
	stubKeyringGOOS(t, goos)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KeyringTool()), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// stubKeyringGOOS makes the keyring plane take goos's branch for the rest of the test.
func stubKeyringGOOS(t *testing.T, goos string) {
	t.Helper()
	orig := keyringGOOS
	keyringGOOS = goos
	t.Cleanup(func() { keyringGOOS = orig })
}

// slowSecretTool points PATH at a stub secret-tool that sleeps past a shortened timeout, so a
// Lookup call times out instead of answering.
func slowSecretTool(t *testing.T) {
	t.Helper()
	fakeKeyringTool(t, "linux", "/bin/sleep 2")
}

// hermetic points PATH at an empty dir so no real `security`/`secret-tool` is ever reachable.
func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("HARNESS_KEYRING", "") // the audit pass forces it off; these tests fake the tool
	t.Setenv("PATH", t.TempDir())
	down.Store(false)
	t.Cleanup(func() { down.Store(false) })
}

// Get/Set on a box with no keyring tool on PATH must degrade, never panic or hang, and
// never touch the real OS keychain regardless of what runs this test.
func TestDegradesWithoutTool(t *testing.T) {
	hermetic(t)
	if KeyringAvailable() {
		t.Fatal("KeyringAvailable() must be false with PATH pointed at an empty dir")
	}
	if _, ok, _ := KeyringLookup("ANYTHING"); ok {
		t.Fatal("Get must miss when no keyring tool is available")
	}
	if err := osKeyringSet("ANYTHING", "v"); err == nil {
		t.Fatal("Set must error when no keyring tool is available")
	}
}

// Lookup must return ErrKeyringNoAnswer, not a plain miss, when the tool times out.
func TestLookupTimesOutReturnsErrNoAnswer(t *testing.T) {
	hermetic(t)
	slowSecretTool(t)
	saved := timeout
	timeout = 50 * time.Millisecond
	t.Cleanup(func() { timeout = saved })

	v, found, err := KeyringLookup("ANYTHING")
	if v != "" || found || err != ErrKeyringNoAnswer {
		t.Fatalf("Lookup = (%q, %v, %v), want (\"\", false, ErrKeyringNoAnswer)", v, found, err)
	}
	if !down.Load() {
		t.Fatal("down must latch true after a timeout")
	}
}

func TestConcurrentLookupSetTimeout(t *testing.T) {
	hermetic(t)
	slowSecretTool(t)
	saved := timeout
	timeout = 10 * time.Millisecond
	t.Cleanup(func() { timeout = saved })

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			<-start
			if _, _, err := KeyringLookup("CONCURRENT"); err != ErrKeyringNoAnswer {
				t.Errorf("Lookup error = %v, want ErrKeyringNoAnswer", err)
			}
		})
		wg.Go(func() {
			<-start
			if err := osKeyringSet("CONCURRENT", "value"); err == nil {
				t.Error("Set must fail when the tool times out")
			}
		})
	}
	close(start)
	wg.Wait()
	if _, _, err := KeyringLookup("CONCURRENT"); err != ErrKeyringNoAnswer {
		t.Errorf("latched Lookup error = %v, want ErrKeyringNoAnswer", err)
	}
}

// Lookup must short-circuit to ErrKeyringNoAnswer, without even running the tool, once down is already set.
func TestLookupLatchedDownReturnsErrNoAnswer(t *testing.T) {
	hermetic(t)
	fakeKeyringTool(t, "linux", "exit 1")
	down.Store(true)
	v, found, err := KeyringLookup("ANYTHING")
	if v != "" || found || err != ErrKeyringNoAnswer {
		t.Fatalf("Lookup = (%q, %v, %v), want (\"\", false, ErrKeyringNoAnswer)", v, found, err)
	}
}

// A plain miss (tool answers, account not stored) must read as ("", false, nil), not ErrKeyringNoAnswer.
func TestLookupPlainMiss(t *testing.T) {
	hermetic(t)
	fakeKeyringTool(t, "linux", "exit 1")

	v, found, err := KeyringLookup("ANYTHING")
	if v != "" || found || err != nil {
		t.Fatalf("Lookup = (%q, %v, %v), want (\"\", false, nil)", v, found, err)
	}
}

func TestSetRejectsEmptyAndMultiline(t *testing.T) {
	hermetic(t)
	if err := osKeyringSet("NAME", ""); err == nil {
		t.Fatal("Set must reject an empty value")
	}
	if err := osKeyringSet("", "v"); err == nil || err.Error() != "keyring: empty name" {
		t.Fatalf("Set with an empty name = %v, want keyring: empty name", err)
	}
	if err := osKeyringSet("NAME", "line1\nline2"); err == nil {
		t.Fatal("Set must reject a value spanning more than one line")
	}
}

func TestSetLatchedDownErrors(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			hermetic(t)
			fakeKeyringTool(t, goos, "exit 99")
			down.Store(true)

			want := "keyring: the keyring did not answer earlier this run — set HARNESS_VAULT_KEY or meta/key_command instead"
			if err := osKeyringSet("NAME", "value"); err == nil || err.Error() != want {
				t.Fatalf("Set with a latched-down keyring = %v, want %q", err, want)
			}
		})
	}
}

// Each OS branch names its own tool and keeps the secret off argv; an OS with no tool degrades to errKeyringNoTool.
func TestKeyringCommandsPerOS(t *testing.T) {
	for _, tc := range []struct {
		goos, tool, lookup, store, stdin string
	}{
		{"linux", "secret-tool", "secret-tool lookup service claude-code account NAME",
			"secret-tool store --label=claude-code service claude-code account NAME", "s3cret"},
		{"darwin", "security", "security find-generic-password -a NAME -s claude-code -w",
			"security -i", "add-generic-password -U -a \"NAME\" -s claude-code -w \"s3cret\"\n"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			stubKeyringGOOS(t, tc.goos)
			if got := KeyringTool(); got != tc.tool {
				t.Fatalf("KeyringTool() = %q, want %q", got, tc.tool)
			}
			if got := strings.Join(lookupCommand(t.Context(), "NAME").Args, " "); got != tc.lookup {
				t.Errorf("lookup argv = %q, want %q", got, tc.lookup)
			}
			cmd := storeCommand(t.Context(), "NAME", "s3cret")
			if got := strings.Join(cmd.Args, " "); got != tc.store {
				t.Errorf("store argv = %q, want %q", got, tc.store)
			}
			if stdin, _ := io.ReadAll(cmd.Stdin); string(stdin) != tc.stdin {
				t.Errorf("store stdin = %q, want %q", stdin, tc.stdin)
			}
		})
	}
	t.Run("windows", func(t *testing.T) {
		hermetic(t)
		stubKeyringGOOS(t, "windows")
		if KeyringTool() != "" || KeyringAvailable() {
			t.Fatalf("KeyringTool() = %q, KeyringAvailable() = %v; want none on windows", KeyringTool(), KeyringAvailable())
		}
		if _, _, err := KeyringLookup("NAME"); !errors.Is(err, errKeyringNoTool) {
			t.Fatalf("Lookup = %v, want errKeyringNoTool", err)
		}
	})
}

// quote must escape a backslash and a double quote so the `security -i` tokenizer sees one argument.
func TestQuoteEscapesBackslashAndDoubleQuote(t *testing.T) {
	got := quote(`a\b"c`)
	want := `"a\\b\"c"`
	if got != want {
		t.Errorf("quote(%q) = %q, want %q", `a\b"c`, got, want)
	}
}

func TestLookupToolFailureIsUnavailable(t *testing.T) {
	for goos, missCode := range map[string]string{"linux": "1", "darwin": "44"} {
		t.Run(goos, func(t *testing.T) { lookupToolFailureCases(t, goos, missCode) })
	}
}

func lookupToolFailureCases(t *testing.T, goos, missCode string) {
	for _, tc := range []struct {
		name, script string
		value        string
		want         error
		timeout      time.Duration
	}{
		{"found", "printf '%s\\n' representative-key", "representative-key", nil, 0},
		{"absent", "exit " + missCode, "", nil, 0},
		{"stderr_failure", "echo 'fake keyring unavailable' >&2\nexit 1", "", ErrKeyringUnavailable, 0},
		{"other_exit", "exit 2", "", ErrKeyringUnavailable, 0},
		{"hanging", "exec /bin/sleep 2", "", ErrKeyringNoAnswer, 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hermetic(t)
			fakeKeyringTool(t, goos, tc.script)
			saved := timeout
			// 1s, not 50ms: under load, forking /bin/sh for the fixture script itself can take
			// tens to hundreds of ms, and a too-tight timeout flakes the fast-path subtests
			// ("found"/"absent"/etc.) with a false ErrKeyringNoAnswer; "hanging" (sleep 2) still times
			// out well inside that budget, so what this asserts is unchanged.
			timeout = time.Second
			if tc.timeout > 0 {
				timeout = tc.timeout
			}
			t.Cleanup(func() { timeout = saved })

			value, found, err := KeyringLookup("PHASE1B")
			if value != tc.value || found != (tc.value != "") || !errors.Is(err, tc.want) {
				t.Fatalf("Lookup = (%q, %t, %v), want (%q, %t, %v)",
					value, found, err, tc.value, tc.value != "", tc.want)
			}
		})
	}
}

func TestLookupNoToolIsUnavailable(t *testing.T) {
	hermetic(t)
	value, found, err := KeyringLookup("PHASE1B")
	if value != "" || found || !errors.Is(err, ErrKeyringUnavailable) {
		t.Fatalf("Lookup without tool = (%q, %t, %v), want empty, false, ErrKeyringUnavailable", value, found, err)
	}
}

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
