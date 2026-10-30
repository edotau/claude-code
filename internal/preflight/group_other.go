//go:build !unix

package preflight

import "os/exec"

func groupKill(*exec.Cmd) {}
