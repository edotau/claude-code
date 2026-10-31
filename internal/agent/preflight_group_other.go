//go:build !unix

package agent

import "os/exec"

func groupKill(*exec.Cmd) {}
