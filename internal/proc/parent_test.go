//go:build unix

package proc

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestDescendants(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & sleep 30 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var got []int
	t.Cleanup(func() {
		for _, pid := range got {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		for _, pid := range Descendants(cmd.Process.Pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got = Descendants(cmd.Process.Pid)
		if len(got) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 2 {
		t.Fatalf("Descendants(%d) = %v, want 2 sleep PIDs", cmd.Process.Pid, got)
	}
	for _, pid := range got {
		if len(Descendants(pid)) != 0 {
			t.Errorf("Descendants(leaf %d) = %v, want empty", pid, Descendants(pid))
		}
	}
}
