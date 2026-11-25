//go:build unix

package proc

import (
	"os/exec"
	"syscall"
	"time"
)

// Detach starts cmd in its own session, untagged, so it outlives the parent's terminal and any TreeCancel above it.
func Detach(cmd *exec.Cmd) {
	setsid(cmd)
	cmd.Env = untagged(cmd.Env)
}

func setsid(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// OwnGroup runs cmd in its own process group and kills the whole group on cancel.
func OwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		go ReapGroup(pgid, groupSweep) // a fork in flight at cancel escapes the kill; Wait reaps the leader and ends the sweep
		return err
	}
}

// groupSweep caps the post-cancel sweep of a group; a leader not yet reaped keeps the group visible until Wait.
const groupSweep = time.Second

// execImage replaces the current process image so the child owns the TTY and signals; returns only on failure.
var execImage = func(bin string, argv, env []string) error {
	return syscall.Exec(bin, append([]string{bin}, argv...), env)
}
