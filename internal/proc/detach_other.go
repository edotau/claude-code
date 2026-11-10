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

// execImage runs bin as a child and exits with its status (no execve off unix).
var execImage = func(bin string, argv, env []string) error {
	code, err := RunForwarding(bin, argv, env, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}
