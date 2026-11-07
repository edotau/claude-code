//go:build !unix

package worktree

import "os/exec"

// killGroupOnCancel is a no-op where process groups are unavailable; CommandContext kills the process itself.
func killGroupOnCancel(*exec.Cmd) {}
