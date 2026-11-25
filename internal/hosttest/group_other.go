//go:build !unix

package hosttest

import "os/exec"

func ownGroup(*exec.Cmd) {}
