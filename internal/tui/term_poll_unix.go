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

// waitReadable reports whether fd has input within d. Anything it cannot answer (out-of-range fd, an
// unexpected error) reports true, so the caller falls back to a plain blocking read.
func waitReadable(fd int, d time.Duration) bool {
	if fd < 0 || fd >= fdSetSize {
		return true
	}
	var set syscall.FdSet
	nbits := fdSetSize / len(set.Bits)
	set.Bits[fd/nbits] |= 1 << uint(fd%nbits)
	tv := syscall.NsecToTimeval(int64(d))
	if err := sysSelect(fd+1, &set, &tv); err != nil {
		return err != syscall.EINTR // EINTR (Go's preemption signal) is just a short poll — go round again
	}
	return set.Bits[fd/nbits]&(1<<uint(fd%nbits)) != 0
}
