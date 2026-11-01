//go:build unix

package vscode

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// fakeServeWebArgv makes procCmdline report pid as our `code serve-web` on port, restoring the real reader after.
func fakeServeWebArgv(t *testing.T, pid, port int) {
	t.Helper()
	orig := procCmdline
	line := strings.Join(ServeWebArgv("code", "/tmp/secret", port, "/tmp/data", ""), " ")
	procCmdline = func(p int) (string, error) {
		if p == pid {
			return line, nil
		}
		return orig(p)
	}
	t.Cleanup(func() { procCmdline = orig })
}

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
	port := unusedPort(t)
	writeStateFixture(t, State{PID: cmd.Process.Pid, Port: port})
	fakeServeWebArgv(t, cmd.Process.Pid, port)

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

// A live pid whose real command line is not our serve-web (a recycled pid) is never signalled; its stale state goes.
func TestStopLeavesRecycledPidAlone(t *testing.T) {
	setup(t)
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	writeStateFixture(t, State{PID: cmd.Process.Pid, Port: DefaultPort})

	if err := Stop(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop: err = %v, want ErrNotRunning", err)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("Stop killed an unrelated live process: %v", err)
	}
	if _, err := os.Stat(stateFile()); !os.IsNotExist(err) {
		t.Fatalf("stale state file still present (err=%v)", err)
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
	port := unusedPort(t)
	writeStateFixture(t, State{PID: cmd.Process.Pid, Port: port})
	fakeServeWebArgv(t, cmd.Process.Pid, port)

	if err := Stop(); err == nil {
		t.Fatal("Stop: want an error for a daemon that outlives SIGTERM")
	}
	if _, err := os.Stat(stateFile()); err != nil {
		t.Fatalf("state file must be kept while the daemon lives: %v", err)
	}
}
