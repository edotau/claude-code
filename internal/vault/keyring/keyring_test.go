package keyring

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
	if Available() {
		t.Fatal("Available() must be false with PATH pointed at an empty dir")
	}
	if _, ok := Get("ANYTHING"); ok {
		t.Fatal("Get must miss when no keyring tool is available")
	}
	if err := Set("ANYTHING", "v"); err == nil {
		t.Fatal("Set must error when no keyring tool is available")
	}
	if err := Delete("ANYTHING"); err != nil {
		t.Fatalf("Delete must be a no-op, not an error, when no keyring tool is available: %v", err)
	}
}

// Lookup must return ErrNoAnswer, not a plain miss, when the tool times out.
func TestLookupTimesOutReturnsErrNoAnswer(t *testing.T) {
	hermetic(t)
	slowSecretTool(t)
	saved := timeout
	timeout = 50 * time.Millisecond
	t.Cleanup(func() { timeout = saved })

	v, found, err := Lookup("ANYTHING")
	if v != "" || found || err != ErrNoAnswer {
		t.Fatalf("Lookup = (%q, %v, %v), want (\"\", false, ErrNoAnswer)", v, found, err)
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
			if _, _, err := Lookup("CONCURRENT"); err != ErrNoAnswer {
				t.Errorf("Lookup error = %v, want ErrNoAnswer", err)
			}
		})
		wg.Go(func() {
			<-start
			if err := Set("CONCURRENT", "value"); err == nil {
				t.Error("Set must fail when the tool times out")
			}
		})
		wg.Go(func() {
			<-start
			if err := Delete("CONCURRENT"); err == nil {
				t.Error("Delete must fail when the tool times out")
			}
		})
	}
	close(start)
	wg.Wait()
	if _, _, err := Lookup("CONCURRENT"); err != ErrNoAnswer {
		t.Errorf("latched Lookup error = %v, want ErrNoAnswer", err)
	}
}

// Lookup must short-circuit to ErrNoAnswer, without even running the tool, once down is already set.
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
	v, found, err := Lookup("ANYTHING")
	if v != "" || found || err != ErrNoAnswer {
		t.Fatalf("Lookup = (%q, %v, %v), want (\"\", false, ErrNoAnswer)", v, found, err)
	}
}

// A plain miss (tool answers, account not stored) must read as ("", false, nil), not ErrNoAnswer.
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

	v, found, err := Lookup("ANYTHING")
	if v != "" || found || err != nil {
		t.Fatalf("Lookup = (%q, %v, %v), want (\"\", false, nil)", v, found, err)
	}
}

func TestSetRejectsEmptyAndMultiline(t *testing.T) {
	hermetic(t)
	if err := Set("NAME", ""); err == nil {
		t.Fatal("Set must reject an empty value")
	}
	if err := Set("", "v"); err == nil || err.Error() != "keyring: empty name" {
		t.Fatalf("Set with an empty name = %v, want keyring: empty name", err)
	}
	if err := Set("NAME", "line1\nline2"); err == nil {
		t.Fatal("Set must reject a value spanning more than one line")
	}
}

func TestSetDeleteLatchedDownErrors(t *testing.T) {
	hermetic(t)
	if Tool() == "" {
		t.Skip("no keyring tool is implemented for this OS")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Tool()), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	down.Store(true)

	for _, tt := range []struct {
		name string
		run  func() error
		want string
	}{
		{"Set", func() error { return Set("NAME", "value") }, "keyring: the keyring did not answer earlier this run — set HARNESS_VAULT_KEY or meta/key_command instead"},
		{"Delete", func() error { return Delete("NAME") }, "keyring: the keyring did not answer earlier this run"},
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
