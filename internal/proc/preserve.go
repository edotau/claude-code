//go:build linux || darwin

package proc

import (
	"os"
	"syscall"
	"unsafe"
)

func ioctlTermios(fd int, req uint, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

func isTerminal(fd int) bool {
	var t syscall.Termios
	return ioctlTermios(fd, ioctlGet, &t) == nil
}

// Preserve snapshots stdin's termios; the returned func restores it and re-shows the cursor (no-op off a tty).
func Preserve() (restore func()) {
	if !isTerminal(0) {
		return func() {}
	}
	var saved syscall.Termios
	if ioctlTermios(0, ioctlGet, &saved) != nil {
		return func() {}
	}
	showCursor := isTerminal(1)
	return func() {
		_ = ioctlTermios(0, ioctlSet, &saved)
		if showCursor {
			_, _ = os.Stdout.WriteString("\x1b[?25h")
		}
	}
}
