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
