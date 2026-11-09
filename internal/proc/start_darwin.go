//go:build darwin

package proc

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// StartTime returns an opaque token identifying pid's start (ps lstart).
func StartTime(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if ctx.Err() != nil { // a killed ps exits non-zero too; report the timeout, not "no such process"
		return "", ctx.Err()
	}
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
