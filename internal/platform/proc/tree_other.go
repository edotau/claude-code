//go:build !unix

package proc

import (
	"os/exec"
	"strconv"
	"time"
)

// TreeCancel kills cmd and every descendant with taskkill /T on cancel; grace is unused (no graceful signal).
func TreeCancel(cmd *exec.Cmd, _ time.Duration) {
	cmd.Cancel = func() error {
		_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}

// TreeSweep is a no-op off unix: there is no env tag to sweep by.
func TreeSweep(*exec.Cmd) error { return nil }

// TreeTag is "" off unix: TreeCancel tags nothing there.
func TreeTag(*exec.Cmd) string { return "" }

// TreeAlive is false off unix: there is no tag to find.
func TreeAlive(string) (bool, error) { return false, nil }

// ReapGroup is a no-op off unix: taskkill /T already took the whole tree.
func ReapGroup(int, time.Duration) bool { return true }
