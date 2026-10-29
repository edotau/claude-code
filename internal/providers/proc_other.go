//go:build !unix

package providers

import "os/exec"

func outputInGroup(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }
