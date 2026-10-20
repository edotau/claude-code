// Package proc owns process replacement (launch handoff) and detached spawns.
package proc

import (
	"context"
	"os"
	"os/exec"
)

// SpawnDetached starts bin+args in its own session, output on out (nil = discarded), never waited on.
// Background(): the spawned process outlives this call by design, so there is nothing to cancel.
func SpawnDetached(bin string, args []string, dir string, out *os.File, extraEnv ...string) bool {
	cmd := exec.CommandContext(context.Background(), bin, args...)
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
