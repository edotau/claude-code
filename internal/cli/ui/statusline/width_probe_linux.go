//go:build linux

package statusline

import (
	"context"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// psParentTTY reads pid's parent and controlling tty without spawning a process.
func psParentTTY(ctx context.Context, pid int) (int, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	if pid <= 0 {
		return 0, "", unix.ESRCH
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", err
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	return parseProcStat(string(data))
}
