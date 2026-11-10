//go:build unix

package memory

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

const (
	lockedReadTimeout = 100 * time.Millisecond  // shrinks vault.View's lock wait for the test
	lockedReadBound   = 2500 * time.Millisecond // the ~2s vault.View timeout plus scheduling slack
)

// TestBankLockPathLocation pins the physical lock file and guards against an accidental ".lock.lock" variant.
func TestBankLockPathLocation(t *testing.T) {
	cfg := tempConfig(t)
	p := BankLockPath()
	want := filepath.Join(cfg, "state", "harness", "memory", "bank.lock")
	if p != want {
		t.Fatalf("BankLockPath() = %q, want %q", p, want)
	}
	if strings.HasSuffix(p, ".lock.lock") {
		t.Fatalf("BankLockPath() produced a .lock.lock variant: %q", p)
	}
	if err := WithBankLock(10*time.Millisecond, func() error { return nil }); err != nil {
		t.Fatalf("WithBankLock: %v", err)
	}
	if _, err := os.Stat(p + ".lock"); err == nil {
		t.Fatalf("a %s.lock file must never appear", p)
	}
}

// TestWithBankLockContention mirrors paths.TestTryFileLockContention: another fd holding the flock must
// block WithBankLock out with ErrBankBusy and never run fn, then let it through once released.
func TestWithBankLockContention(t *testing.T) {
	tempConfig(t)
	path := BankLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	ran := false
	err = WithBankLock(10*time.Millisecond, func() error { ran = true; return nil })
	if err != ErrBankBusy {
		t.Fatalf("WithBankLock while held = %v, want ErrBankBusy", err)
	}
	if ran {
		t.Fatal("fn must not run while the bank lock is held elsewhere")
	}

	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()

	ran = false
	if err := WithBankLock(10*time.Millisecond, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("WithBankLock after release: %v", err)
	}
	if !ran {
		t.Fatal("fn must run once the bank lock is released")
	}
}

// seedLockedCatalog writes an index.md row whose origin only the catalog can supply.
func seedLockedCatalog(t *testing.T) Origin {
	t.Helper()
	cfg := tempConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg, "memory", "locked"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := Origin{Version: OriginVersion, Key: "locked", Origin: "/src/locked"}
	row := "- locked — hook · last 2024-01-01 → locked/MEMORY.md" + CatalogOriginComment(want) + "\n"
	write(t, filepath.Join(cfg, "memory", CatalogFile), "# Memory catalog\n\n## Repos\n\n"+row)
	return want
}

// holdVaultFlock takes a real OS LOCK_EX on vault.db, creating the file first, and returns its release.
func holdVaultFlock(t *testing.T) (release func()) {
	t.Helper()
	if err := vault.Update(func(*vault.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(vault.Path(), syscall.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
	}
}

// TestVaultLockedFallsBackToCatalogThenSkipsRetryFor30s: under a real OS lock, RepoDirs reads index.md, then backs off.
func TestVaultLockedFallsBackToCatalogThenSkipsRetryFor30s(t *testing.T) {
	resetVaultRetry(t)
	t.Cleanup(vault.SetReadLockTimeoutForTest(lockedReadTimeout))
	want := seedLockedCatalog(t)
	release := holdVaultFlock(t)

	start := time.Now()
	entries, err := RepoDirs()
	elapsed := time.Since(start)
	if err != nil || len(entries) != 1 || entries[0].Origin.Origin != want.Origin {
		t.Fatalf("RepoDirs under an external lock = %+v, %v, want the locked entry", entries, err)
	}
	if elapsed > lockedReadBound {
		t.Errorf("RepoDirs under lock took %v, want bounded by the ~2s vault.View timeout", elapsed)
	}

	start = time.Now()
	if _, err := RepoDirs(); err != nil {
		t.Fatal(err)
	}
	if again := time.Since(start); again > time.Second {
		t.Errorf("second call within the 30s backoff took %v, want near-instant (no repeat wait)", again)
	}

	release() // vault.View would block while locked, so the no-write check has to follow this
	if _, ok := vaultGet(t, "locked"); ok {
		t.Error("a locked vault.View must never be followed by a write attempt")
	}
}
