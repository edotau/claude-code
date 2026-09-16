//go:build !unix

package proc

import (
	"errors"
	"os"
	"os/exec"
)

// Detach is a no-op off unix.
func Detach(*exec.Cmd) {}

// Exec runs bin as a child and exits with its status (no execve off unix).
func Exec(bin string, argv, env []string) error {
	cmd := exec.Command(bin, argv...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	if err == nil {
		os.Exit(0)
	}
	return err
}
