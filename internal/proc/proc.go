// Package proc owns process replacement (launch handoff) and detached spawns.
package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// SpawnDetached starts bin+args in its own session, output on out (nil = discarded), never waited on.
// Background(): the spawned process outlives this call by design, so there is nothing to cancel.
func SpawnDetached(bin string, args []string, dir string, out *os.File, extraEnv ...string) bool {
	if IsSelfUnderTest(bin) {
		return false
	}
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

// IsSelfUnderTest: under `go test`, os.Executable is the test binary; relaunching it re-runs every test (fork bomb).
func IsSelfUnderTest(bin string) bool {
	if !testing.Testing() {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return true
	}
	a, _ := filepath.EvalSymlinks(self)
	b, _ := filepath.EvalSymlinks(bin)
	return bin == self || (a != "" && a == b)
}
