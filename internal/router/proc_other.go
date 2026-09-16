//go:build !unix

package router

import (
	"os"
	"os/exec"
)

func detach(*exec.Cmd) {}

func alive(pid int) bool { return pid > 0 }

func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
