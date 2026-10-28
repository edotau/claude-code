//go:build darwin

package proc

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// StartTime returns an opaque token identifying pid's start (ps lstart).
func StartTime(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", errors.New("proc: no such process")
	}
	return s, nil
}
