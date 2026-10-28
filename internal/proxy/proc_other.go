//go:build !unix

package proxy

import "os"

func alive(pid int) bool { return pid > 0 }

func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
