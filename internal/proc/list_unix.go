//go:build unix

package proc

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// List runs `ps axeww` plus `ps -axo pid=,comm=` for executable paths, keeping each row's mark value; empty in tests.
func List(mark string) ([]Info, error) {
	if testing.Testing() {
		return nil, nil
	}
	return listWith(psOutput, mark)
}

func psOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	return string(out), err
}
