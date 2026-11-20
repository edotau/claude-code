//go:build unix

package proc

import (
	"os/exec"
	"syscall"
	"time"
)

// TreeCancel runs cmd in its own group; on cancel it asks for a stop, waits up to grace, then kills every descendant.
func TreeCancel(cmd *exec.Cmd, grace time.Duration) {
	OwnGroup(cmd)
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		kids := Descendants(pid) // snapshot first: a killed parent reparents its children out of reach
		_ = cmd.Process.Signal(syscall.SIGTERM)
		for end := time.Now().Add(grace); time.Now().Before(end) && Alive(pid); {
			time.Sleep(10 * time.Millisecond)
		}
		for _, k := range append(kids, Descendants(pid)...) {
			if Alive(k) {
				_ = SignalGroup(k, syscall.SIGKILL)  // a tool's own group (Pi's bash) is not ours
				_ = syscall.Kill(k, syscall.SIGKILL) // not a group leader: the group signal missed it
			}
		}
		return syscall.Kill(-pid, syscall.SIGKILL)
	}
}
