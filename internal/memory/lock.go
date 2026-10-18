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
const BankLockWait = 5 * time.Second

// ErrBankBusy is returned when the bank lock is not granted within the wait.
var ErrBankBusy = errors.New("memory store busy: another harness worker holds the bank lock")

// BankLockPath is the PHYSICAL lock file; nothing appends ".lock" to it (plan §3.1: no .lock.lock variants).
func BankLockPath() string { return filepath.Join(StateDir(), BankLockFile) }

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
