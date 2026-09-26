package paths

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TryFileLock must not run fn while another fd already holds the lock, and must run it once released.
func TestTryFileLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock(f); err != nil {
		t.Fatal(err)
	}

	ran, err := TryFileLock(path, 100*time.Millisecond, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("TryFileLock must not run fn while the lock is held elsewhere")
	}

	unlock(f)
	f.Close()

	ran, err = TryFileLock(path, 100*time.Millisecond, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("TryFileLock must run fn once the lock is released")
	}
}
