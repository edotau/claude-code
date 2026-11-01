//go:build !unix

package proc

import (
	"os"
	"os/exec"
)

// Detach is a no-op off unix.
func Detach(*exec.Cmd) {}

// OwnGroup is a no-op off unix.
func OwnGroup(*exec.Cmd) {}

// Exec runs bin as a child and exits with its status (no execve off unix).
func Exec(bin string, argv, env []string) error {
	code, err := RunForwarding(bin, argv, env, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}
