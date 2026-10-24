//go:build unix

package vendoragent

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A leader process (its own process group via Setpgid) that ignores SIGINT itself, with a second,
// independent process placed into that same group and trapping SIGINT, proves interrupt(leaderPid)
// reaches the whole group — not just the leader pid passed in. The child is started directly by this
// test (SysProcAttr{Setpgid:true, Pgid:leaderPid}), not via the leader shell's own "&": a POSIX shell
// auto-ignores INT/QUIT for its own asynchronous ("&") children (so their own traps for INT never fire,
// regardless of an empty trap body for INT anywhere), which would make that route defeat this test's premise.
func TestInterruptReachesWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-got-sigint")
	childScript := filepath.Join(dir, "child.sh")
	leaderScript := filepath.Join(dir, "leader.sh")

	childBody := "#!/bin/sh\ntrap 'touch \"" + marker + "\"; exit 0' INT\nsleep 30 &\nwait\n"
	if err := os.WriteFile(childScript, []byte(childBody), 0o700); err != nil {
		t.Fatal(err)
	}
	leaderBody := "#!/bin/sh\ntrap '' INT\nsleep 30 &\nwait\n"
	if err := os.WriteFile(leaderScript, []byte(leaderBody), 0o700); err != nil {
		t.Fatal(err)
	}

	leader := exec.Command(leaderScript)
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	leaderPid := leader.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-leaderPid, syscall.SIGKILL)
		_ = leader.Wait()
	})

	child := exec.Command(childScript)
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leaderPid}
	if err := child.Start(); err != nil {
		t.Fatalf("start child into leader's group: %v", err)
	}

	// give both processes time to install their traps before signalling, else SIGINT can race trap
	// installation (same reasoning as TestCancelEscalatesToSIGKILL in jobs_test.go).
	time.Sleep(200 * time.Millisecond)

	if err := interrupt(leaderPid); err != nil {
		t.Fatalf("interrupt(leaderPid) = %v, want nil", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return // the child, a different pid in the same group, observed the SIGINT.
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child never observed SIGINT: interrupt(leaderPid) did not reach the whole process group")
}

// A pid that has already exited (ESRCH) is success, not an error — the caller never needs to special-case it.
func TestInterruptOnExitedPIDReturnsNil(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := interrupt(cmd.Process.Pid); err != nil {
		t.Fatalf("interrupt(exited pid) = %v, want nil (ESRCH is success)", err)
	}
}

// pid<=1 must never reach Kill(-pid, ...): -1 broadcasts SIGINT/SIGKILL to every process the user owns.
func TestSignalGuardsAgainstGroupSignalForPIDLEQ1(t *testing.T) {
	var got []int
	orig := sysKill
	sysKill = func(pid int, sig syscall.Signal) error {
		got = append(got, pid)
		return syscall.EPERM
	}
	t.Cleanup(func() { sysKill = orig })

	for _, fn := range []func(int) error{interrupt, kill} {
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
