//go:build linux || darwin

package statusline

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// ancestorTTYWidth is the column count of the first ancestor process attached to a terminal (0 if none).
func ancestorTTYWidth() int {
	pid := os.Getppid()
	for hop := 0; hop < 8 && pid > 1; hop++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=,tty=", "-p", strconv.Itoa(pid)).Output()
		cancel()
		f := strings.Fields(string(out))
		if err != nil || len(f) < 1 {
			return 0
		}
		if len(f) > 1 && !strings.HasPrefix(f[1], "?") && f[1] != "-" {
			if w := ttyCols("/dev/" + f[1]); w > 0 {
				return w
			}
		}
		pid, _ = strconv.Atoi(f[0])
	}
	return 0
}

func ttyCols(dev string) int {
	f, err := os.Open(dev)
	if err != nil {
		return 0
	}
	defer f.Close()
	cols, _, err := proc.Size(int(f.Fd()))
	if err != nil {
		return 0
	}
	return cols
}
