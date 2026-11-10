package gateway

import (
	"os/exec"
	"testing"
	"time"
)

func TestAwaitExitObservesTerminatedWindowsProcess(t *testing.T) {
	cmd := exec.Command("ping.exe", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if err := cmd.Process.Kill(); err != nil {
				t.Error(err)
			}
			_ = cmd.Wait()
		}
	})
	pid := cmd.Process.Pid
	if !alive(pid) {
		t.Fatal("running child process reported dead")
	}
	if err := terminate(pid); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("terminated child unexpectedly exited successfully")
	}
	previous := ShutdownDrain
	ShutdownDrain = -4 * time.Second
	t.Cleanup(func() { ShutdownDrain = previous })
	if !awaitExit(pid) {
		t.Fatal("awaitExit still reports a terminated process alive")
	}
}
