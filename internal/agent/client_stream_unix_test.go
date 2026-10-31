//go:build unix

package agent

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Given a real child under childCmd with a grandchild, When ctx is cancelled, Then no process stays in the group.
func TestChildCmdCancelLeavesNoProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, _ := childCmd(ctx, "sh", []string{"-c", "sleep 60 & sleep 60; wait"}, nil, t.TempDir(), "", nil, false)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("group %d not alive before cancel: %v", pgid, err)
	}
	cancel()
	var ee *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &ee) {
		t.Fatalf("wait = %v, want the kill's exit error", err)
	}
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return
		}
	}
	t.Errorf("process group %d still has members after cancel", pgid)
}
