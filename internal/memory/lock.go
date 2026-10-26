package memory

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// BankLockFile is the ONE store-wide lock; every harness write and every memory Git operation runs under it.
const BankLockFile = "bank.lock"

// BankLockWait bounds how long a worker waits for the bank before giving up with ErrBankBusy.
var BankLockWait = 5 * time.Second

// ErrBankBusy is returned when the bank lock is not granted within the wait.
var ErrBankBusy = errors.New("memory store busy: another harness worker holds the bank lock")

// BankLockPath is the PHYSICAL lock file; nothing appends ".lock" to it (plan §3.1: no .lock.lock variants).
func BankLockPath() string { return filepath.Join(StateDir(), BankLockFile) }

// storeLockKey namespaces a bank dir's runtime lock files under state: the store dir's base name,
// or "home" for the home bank itself — locks never live beside the committed memory store.
func storeLockKey(dir string) string {
	if paths.SamePath(dir, HomeBankDir()) {
		return "home"
	}
	return filepath.Base(dir)
}

// storeLockDir is state's per-key lock directory for dir's bank, created 0700 on first use.
func storeLockDir(dir string) string {
	d := filepath.Join(StateDir(), "locks", storeLockKey(dir))
	_ = os.MkdirAll(d, 0o700)
	return d
}

// WithBankLock runs fn holding the bank lock; wait<=0 means BankLockWait. Lock order: worker claim → bank → git index.
// Not reentrant: *Locked helpers never take it, so settlement cannot deadlock on itself.
func WithBankLock(wait time.Duration, fn func() error) error {
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	if wait <= 0 {
		wait = BankLockWait
	}
	ran, err := paths.TryFileLock(BankLockPath(), wait, fn)
	if err == nil && !ran {
		return ErrBankBusy
	}
	return err
}
