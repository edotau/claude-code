package worktree

import (
	"testing"
	"time"
)

func mergeFixtureLockEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestAcquireRunsFnAndReleases(t *testing.T) {
	mergeFixtureLockEnv(t)
	ran := false
	if err := Acquire(time.Second, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run")
	}
	// The lock must be released: a second Acquire must succeed promptly, not wait out MergeLockWait.
	start := time.Now()
	if err := Acquire(time.Second, func() error { return nil }); err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("second Acquire took %v, want near-instant (lock should have been released)", elapsed)
	}
}

func TestAcquirePropagatesFnError(t *testing.T) {
	mergeFixtureLockEnv(t)
	sentinel := errFixtureSentinel{}
	err := Acquire(time.Second, func() error { return sentinel })
	if err != sentinel {
		t.Fatalf("err = %v, want the sentinel fn error", err)
	}
}

type errFixtureSentinel struct{}

func (errFixtureSentinel) Error() string { return "fixture sentinel error" }

func TestAcquireBusyReturnsErrMergeBusyWithinWait(t *testing.T) {
	mergeFixtureLockEnv(t)
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = Acquire(2*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	start := time.Now()
	wait := 100 * time.Millisecond
	err := Acquire(wait, func() error {
		t.Fatal("fn must not run while another holder has the lock")
		return nil
	})
	elapsed := time.Since(start)
	if err != ErrMergeBusy {
		t.Fatalf("err = %v, want ErrMergeBusy", err)
	}
	if elapsed < wait {
		t.Fatalf("Acquire returned after %v, want it to have waited at least %v", elapsed, wait)
	}
}

func TestAcquireZeroWaitDefaultsToMergeLockWait(t *testing.T) {
	mergeFixtureLockEnv(t)
	if MergeLockWait != 5*time.Second || mergeLockWait != MergeLockWait {
		t.Fatalf("defaults: MergeLockWait=%v mergeLockWait=%v, want both 5s", MergeLockWait, mergeLockWait)
	}
	const short = 100 * time.Millisecond
	t.Cleanup(func() { mergeLockWait = MergeLockWait })
	mergeLockWait = short
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = Acquire(time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	start := time.Now()
	err := Acquire(0, func() error { return nil })
	elapsed := time.Since(start)
	if err != ErrMergeBusy {
		t.Fatalf("err = %v, want ErrMergeBusy", err)
	}
	// wait<=0 must fall back to the default wait, not return immediately as a zero wait would imply.
	if elapsed < short-20*time.Millisecond {
		t.Fatalf("Acquire(0, ...) returned after %v, want it to have used the default wait (%v)", elapsed, short)
	}
}
