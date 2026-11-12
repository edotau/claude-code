//go:build unix

package proc

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// List runs `ps axeww` (env follows the command) plus `ps -axo pid=,comm=` for executable paths, and keeps each
// row's mark value; empty under `go test`.
func List(mark string) ([]Info, error) {
	if testing.Testing() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := psOutput(ctx, "axeww", "-o", "pid=,ppid=,pgid=,lstart=,command=")
	if err != nil {
		return nil, err
	}
	commOut, err := psOutput(ctx, "-axo", "pid=,comm=")
	if err != nil {
		return nil, err
	}
	comms := parseComms(commOut)
	if runtime.GOOS == "linux" { // comm is the 15-char task name there; /proc/<pid>/exe has the full path
		for pid := range comms {
			if exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
				comms[pid] = exe
			}
		}
	}
	return parseList(out, comms, mark, time.Local), nil
}

func psOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	return string(out), err
}
