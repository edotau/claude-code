//go:build !unix

package providers

import "os/exec"

func ownGroup(cmd *exec.Cmd) {}
