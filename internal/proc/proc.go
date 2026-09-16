// Package proc owns process replacement (launch handoff) and detached spawns.
package proc

import (
	"os"
	"os/exec"
)

// SpawnDetached starts bin+args in its own session, output on out (nil = discarded), never waited on.
func SpawnDetached(bin string, args []string, dir string, out *os.File, extraEnv ...string) bool {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	cmd.Stdout, cmd.Stderr = out, out
	Detach(cmd)
	if cmd.Start() != nil {
		return false
	}
	go func() { _ = cmd.Wait() }() // reap so a long-lived parent never keeps a zombie
	return true
}
