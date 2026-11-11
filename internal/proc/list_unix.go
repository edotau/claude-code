//go:build unix

package proc

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// List runs `ps axeww` (env follows the command) and keeps each row's mark value; empty under `go test`.
func List(mark string) ([]Info, error) {
	if testing.Testing() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "axeww", "-o", "pid=,ppid=,pgid=,lstart=,command=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseList(string(out), mark, time.Local), nil
}
