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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,tty=").Output()
	if err != nil {
		return 0
	}
	type entry struct{ ppid, tty string }
	table := map[string]entry{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			p := entry{ppid: f[1]}
			if len(f) > 2 {
				p.tty = f[2]
			}
			table[f[0]] = p
		}
	}
	pid := os.Getppid()
	for hop := 0; hop < 8 && pid > 1; hop++ {
		p, ok := table[strconv.Itoa(pid)]
		if !ok {
			return 0
		}
		if p.tty != "" && !strings.HasPrefix(p.tty, "?") && p.tty != "-" {
			if w := ttyCols("/dev/" + p.tty); w > 0 {
				return w
			}
		}
		pid, _ = strconv.Atoi(p.ppid)
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
