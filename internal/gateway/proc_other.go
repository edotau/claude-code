//go:build !unix

package gateway

import "os"

// alive reports a live pid: on Windows FindProcess fails once the process object is gone.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer func() { _ = p.Release() }() // an open handle keeps the exited process object, so alive() would stay true
	return p.Kill()
}
