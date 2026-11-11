package proc

import (
	"syscall"
	"testing"
)

// pid<=1 must never reach Kill(-pid, ...): -1 broadcasts SIGINT/SIGKILL to every process the user owns.
func TestSignalGroupGuardsAgainstGroupSignalForPIDLEQ1(t *testing.T) {
	var got []int
	orig := sysKill
	sysKill = func(pid int, sig syscall.Signal) error {
		got = append(got, pid)
		return syscall.EPERM
	}
	t.Cleanup(func() { sysKill = orig })

	for _, fn := range []func(int) error{
		func(pid int) error { return SignalGroup(pid, syscall.SIGINT) },
		func(pid int) error { return SignalGroup(pid, syscall.SIGKILL) },
	} {
		got = nil
		_ = fn(1)
		if len(got) != 1 || got[0] != 1 {
			t.Fatalf("fn(1) signalled %v, want [1] (never -1)", got)
		}

		got = nil
		_ = fn(2)
		if len(got) != 2 || got[0] != -2 || got[1] != 2 {
			t.Fatalf("fn(2) signalled %v, want [-2 2]", got)
		}
	}
}
