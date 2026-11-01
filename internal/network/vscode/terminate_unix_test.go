//go:build unix

package vscode

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// A live daemon that fails its health probe (nothing listens on the port) must still be terminated.
func TestStopTerminatesUnhealthyLiveDaemon(t *testing.T) {
	setup(t)
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }() // reap so proc.Alive sees the exit
	writeStateFixture(t, State{PID: cmd.Process.Pid, Port: unusedPort(t)})

	if err := Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-done:
	default:
		t.Fatal("Stop returned nil but the unhealthy daemon is still running")
	}
	if _, err := os.Stat(stateFile()); !os.IsNotExist(err) {
		t.Fatalf("state file still present after a completed stop (err=%v)", err)
	}
}

// A daemon that ignores SIGTERM keeps its state file and Stop reports the failure.
func TestStopKeepsStateWhenDaemonSurvives(t *testing.T) {
	setup(t)
	cmd := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 1; done")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sh: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	writeStateFixture(t, State{PID: cmd.Process.Pid, Port: unusedPort(t)})

	if err := Stop(); err == nil {
		t.Fatal("Stop: want an error for a daemon that outlives SIGTERM")
	}
	if _, err := os.Stat(stateFile()); err != nil {
		t.Fatalf("state file must be kept while the daemon lives: %v", err)
	}
}
