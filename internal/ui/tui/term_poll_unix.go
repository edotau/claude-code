//go:build linux || darwin

package tui

import (
	"syscall"
	"time"
)

// pollTick bounds how long the key pump parks in select(2) before re-checking stop — the worst-case
// Close latency. A keystroke still wakes the wait immediately, so input latency is untouched.
const pollTick = 50 * time.Millisecond

// canPoll says waitReadable really waits, so Close can join the pump instead of orphaning it.
const canPoll = true

// fdSetSize is FD_SETSIZE: syscall.FdSet holds exactly this many bits (int64[16] or int32[32]).
const fdSetSize = 1024

// pollOnce is one select(2) on fd bounded by d; tests wrap it to interrupt the wait.
var pollOnce = selectOnce

// waitReadable reports whether fd has input within d. Anything it cannot answer (out-of-range fd, an
// unexpected error) reports true, so the caller falls back to a plain blocking read.
func waitReadable(fd int, d time.Duration) bool {
	if fd < 0 || fd >= fdSetSize {
		return true
	}
	deadline := time.Now().Add(d)
	for {
		readable, err := pollOnce(fd, max(time.Until(deadline), 0)) // EINTR retries wait only the remainder
		if err == syscall.EINTR {
			continue // a signal interrupted the wait, not the timeout: select again with a fresh set
		}
		if err != nil {
			return true
		}
		return readable
	}
}

// selectOnce waits up to d for fd to become readable; an interrupted wait returns syscall.EINTR.
func selectOnce(fd int, d time.Duration) (bool, error) {
	nbits := fdSetSize / len(syscall.FdSet{}.Bits)
	var set syscall.FdSet
	set.Bits[fd/nbits] |= 1 << uint(fd%nbits)
	tv := syscall.NsecToTimeval(int64(d))
	err := sysSelect(fd+1, &set, &tv)
	return set.Bits[fd/nbits]&(1<<uint(fd%nbits)) != 0, err
}
