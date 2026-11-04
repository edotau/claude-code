package configure

import (
	"os/exec"
	"testing"
	"time"
)

// An agy that ignores SIGINT and SIGTERM is SIGKILLed after the grace, so stopAgy returns instead of hanging.
func TestStopAgyEscalatesToKill(t *testing.T) {
	orig := AgyStopGrace
	AgyStopGrace = 100 * time.Millisecond
	t.Cleanup(func() { AgyStopGrace = orig })
	cmd := exec.Command("sh", "-c", `trap "" INT TERM; echo ready; while :; do sleep 0.05; done`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Read(make([]byte, 6)); err != nil { // "ready\n": the traps are installed
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stopped := make(chan struct{})
	go func() { stopAgy(cmd, done); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("stopAgy did not return for an agy that ignores SIGINT and SIGTERM")
	}
}
