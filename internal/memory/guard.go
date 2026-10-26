package memory

import (
	"errors"
	"fmt"
	"time"
)

// Guard is the readiness verdict every hook/worker consults before initializing, spawning, stamping or writing.
type Guard struct {
	Notice string
	Paused bool // no write, spawn, stamp or scaffold until the notice's condition clears
}

// ErrMemoryPaused wraps every refusal from CheckWritable; errors.Is lets callers stay quiet instead of failing loud.
var ErrMemoryPaused = errors.New("memory writes paused")

// StartupGuard pauses writes during maintenance; a pure read.
func StartupGuard() Guard {
	if reason, ok := InMaintenance(); ok {
		if reason == "" {
			reason = "no reason recorded"
		}
		return Guard{Paused: true, Notice: "memory maintenance in progress (" + reason + "): memory scaffolding and harvests are paused until it completes; see " + MaintenancePath()}
	}
	return Guard{}
}

// CheckWritable is the one mutation gate: nil when the store accepts writes, else ErrMemoryPaused with the notice.
func CheckWritable() error {
	if g := StartupGuard(); g.Paused {
		return fmt.Errorf("%w: %s", ErrMemoryPaused, g.Notice)
	}
	return nil
}

// WithWritableBankLock is WithBankLock for mutations; the recheck under the lock catches a marker raised meanwhile.
func WithWritableBankLock(wait time.Duration, fn func() error) error {
	if err := CheckWritable(); err != nil {
		return err
	}
	return WithBankLock(wait, func() error {
		if err := CheckWritable(); err != nil {
			return err
		}
		return fn()
	})
}
