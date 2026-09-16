//go:build !unix

package agent

import "os/exec"

func ownGroup(cmd *exec.Cmd) {}
