//go:build darwin

package proc

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Parent returns pid's parent pid via ps (0 when unreadable): darwin has no /proc to read.
func Parent(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	ppid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid
}

// Comm returns pid's executable name via ps ("" when unreadable); ps prints a path, so base it.
func Comm(pid int) string {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return ""
	}
	return filepath.Base(name)
}
