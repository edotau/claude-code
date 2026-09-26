package proc

import (
	"os"
	"os/exec"
	"testing"
)

// Alive must report the calling process itself as alive, and a reaped child pid as dead.
func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("Alive(own pid) must be true")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn a short-lived process: %v", err)
	}
	if Alive(cmd.Process.Pid) {
		t.Error("Alive(reaped child pid) must be false")
	}
}
