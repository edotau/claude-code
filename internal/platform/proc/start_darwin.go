//go:build darwin

package proc

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// lstartLayout is ps's lstart column (strftime %c in the C and en_US locales).
const lstartLayout = "Mon Jan _2 15:04:05 2006"

// StartTime returns an opaque token identifying pid's start (ps lstart); the kernel table gives it without a ps spawn.
func StartTime(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	switch {
	case err == nil:
		return time.Unix(kp.Proc.P_starttime.Sec, 0).Format(lstartLayout), nil
	case errors.Is(err, unix.EIO): // an empty answer: the pid is not in the table
		return "", ErrNoProcess
	}
	return psStartTime(pid)
}

// psStartTime is StartTime through ps, for a pid the kernel table does not answer for.
func psStartTime(pid int) (string, error) {
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
