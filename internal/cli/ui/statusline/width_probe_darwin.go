//go:build darwin

package statusline

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
)

// psParentTTY reads pid's parent and controlling tty without spawning a process.
func psParentTTY(ctx context.Context, pid int) (int, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	if pid <= 0 {
		return 0, "", unix.ESRCH
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, "", err
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	return int(info.Eproc.Ppid), darwinTTYName(uint32(info.Eproc.Tdev)), nil
}

// darwinTTYName returns a /dev-relative name; NODEV and unrecognized devices have no usable tty.
func darwinTTYName(dev uint32) string {
	major, minor := unix.Major(uint64(dev)), unix.Minor(uint64(dev))
	switch {
	case major == 16:
		return fmt.Sprintf("ttys%03d", minor)
	case major == 4 && minor < 128:
		return fmt.Sprintf("tty%c%x", 'p'+minor/16, minor%16)
	case major == 0 && minor == 0:
		return "console"
	case major == 2 && minor == 0:
		return "tty"
	default:
		return ""
	}
}
