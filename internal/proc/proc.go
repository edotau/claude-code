// Package proc owns process replacement (launch handoff) and detached spawns.
package proc

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
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

// RunForwarding runs bin and returns its exit code, -1 if a signal killed it (err only if it could not start or wait).
func RunForwarding(bin string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(context.Background(), bin, argv...)
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// The tty already delivers SIGINT to the child; the parent only survives it and forwards SIGTERM.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			if s == syscall.SIGTERM {
				_ = cmd.Process.Signal(s)
			}
		}
	}()
	err := cmd.Wait()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), nil
	}
	return 0, err
}
