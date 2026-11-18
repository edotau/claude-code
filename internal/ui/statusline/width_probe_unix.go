//go:build linux || darwin

package statusline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// ancestorTTYWidth is the column count of the first ancestor process attached to a terminal (0 if none).
func ancestorTTYWidth() int { return walkAncestors(os.Getppid(), psParentTTY, ttyCols) }

// psParentTTY asks ps for pid's parent and controlling tty; a missing pid is an error.
func psParentTTY(pid int) (ppid int, tty string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=,tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, "", err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return 0, "", errors.New("no such process")
	}
	ppid, _ = strconv.Atoi(f[0])
	if len(f) > 1 {
		tty = f[1]
	}
	return ppid, tty, nil
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
