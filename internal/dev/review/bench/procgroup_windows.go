package bench

import (
	"os/exec"
	"strconv"
)

// ownGroup kills the whole process tree on cancel; taskkill /T walks the descendants.
func ownGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		pid := strconv.Itoa(cmd.Process.Pid)
		if err := exec.Command("taskkill", "/T", "/F", "/PID", pid).Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
