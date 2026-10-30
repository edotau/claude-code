package vault

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// slowSecretTool points PATH at a stub secret-tool that sleeps past a shortened timeout, so a
// Lookup call times out instead of answering.
func slowSecretTool(t *testing.T) {
	t.Helper()
	t.Setenv("HARNESS_KEYRING", "")
	if runtime.GOOS != "linux" {
		t.Skip("stub only implements the linux secret-tool CLI")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n/bin/sleep 2\n"
	if err := os.WriteFile(filepath.Join(dir, "secret-tool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// hermetic points PATH at an empty dir so no real `security`/`secret-tool` is ever reachable.
func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("HARNESS_KEYRING", "") // the audit pass forces it off; these tests fake the tool
	t.Setenv("PATH", t.TempDir())
	down.Store(false)
	t.Cleanup(func() { down.Store(false) })
}

// Get/Set/Delete on a box with no keyring tool on PATH must degrade, never panic or hang, and
// never touch the real OS keychain regardless of what runs this test.
func TestDegradesWithoutTool(t *testing.T) {
	hermetic(t)
	if KeyringAvailable() {
		t.Fatal("KeyringAvailable() must be false with PATH pointed at an empty dir")
	}
	if _, ok := keyringGet("ANYTHING"); ok {
		t.Fatal("Get must miss when no keyring tool is available")
	}
	if err := KeyringSet("ANYTHING", "v"); err == nil {
		t.Fatal("Set must error when no keyring tool is available")
	}
	if err := KeyringDelete("ANYTHING"); err != nil {
		t.Fatalf("Delete must be a no-op, not an error, when no keyring tool is available: %v", err)
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

func TestConcurrentLookupSetDeleteTimeout(t *testing.T) {
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
			if err := KeyringSet("CONCURRENT", "value"); err == nil {
				t.Error("Set must fail when the tool times out")
			}
		})
		wg.Go(func() {
			<-start
			if err := KeyringDelete("CONCURRENT"); err == nil {
				t.Error("Delete must fail when the tool times out")
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
	if runtime.GOOS != "linux" {
		t.Skip("stub only implements the linux secret-tool CLI")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret-tool"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	down.Store(true)
	v, found, err := KeyringLookup("ANYTHING")
	if v != "" || found || err != ErrKeyringNoAnswer {
		t.Fatalf("Lookup = (%q, %v, %v), want (\"\", false, ErrKeyringNoAnswer)", v, found, err)
	}
}

// A plain miss (tool answers, account not stored) must read as ("", false, nil), not ErrKeyringNoAnswer.
func TestLookupPlainMiss(t *testing.T) {
	hermetic(t)
	if runtime.GOOS != "linux" {
		t.Skip("stub only implements the linux secret-tool CLI")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret-tool"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	v, found, err := KeyringLookup("ANYTHING")
	if v != "" || found || err != nil {
		t.Fatalf("Lookup = (%q, %v, %v), want (\"\", false, nil)", v, found, err)
	}
}

func TestSetRejectsEmptyAndMultiline(t *testing.T) {
	hermetic(t)
	if err := KeyringSet("NAME", ""); err == nil {
		t.Fatal("Set must reject an empty value")
	}
	if err := KeyringSet("", "v"); err == nil || err.Error() != "keyring: empty name" {
		t.Fatalf("Set with an empty name = %v, want keyring: empty name", err)
	}
	if err := KeyringSet("NAME", "line1\nline2"); err == nil {
		t.Fatal("Set must reject a value spanning more than one line")
	}
}

func TestSetDeleteLatchedDownErrors(t *testing.T) {
	hermetic(t)
	if KeyringTool() == "" {
		t.Skip("no keyring tool is implemented for this OS")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KeyringTool()), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	down.Store(true)

	for _, tt := range []struct {
		name string
		run  func() error
		want string
	}{
		{"Set", func() error { return KeyringSet("NAME", "value") }, "keyring: the keyring did not answer earlier this run — set HARNESS_VAULT_KEY or meta/key_command instead"},
		{"Delete", func() error { return KeyringDelete("NAME") }, "keyring: the keyring did not answer earlier this run"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); err == nil || err.Error() != tt.want {
				t.Fatalf("%s with a latched-down keyring = %v, want %q", tt.name, err, tt.want)
			}
		})
	}
}

// quote must escape a backslash and a double quote so the `security -i` tokenizer sees one argument.
func TestQuoteEscapesBackslashAndDoubleQuote(t *testing.T) {
	got := quote(`a\b"c`)
	want := `"a\\b\"c"`
	if got != want {
		t.Errorf("quote(%q) = %q, want %q", `a\b"c`, got, want)
	}
}
