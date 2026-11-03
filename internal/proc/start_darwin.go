//go:build darwin

package proc

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// StartTime returns an opaque token identifying pid's start (ps lstart).
func StartTime(pid int) (string, error) {
	out, err := exec.CommandContext(context.Background(), "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		if _, isExit := errors.AsType[*exec.ExitError](err); err == nil || isExit {
			return "", ErrNoProcess
		}
	}
	if err != nil {
		return "", err
	}
	return s, nil
}
