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

// IsTerminalFD reports whether fd accepts the termios get ioctl, which only tty devices do (not /dev/null or files).
func IsTerminalFD(fd int) bool {
	var t syscall.Termios
	return ioctlTermios(fd, ioctlGet, &t) == nil
}

// IsTerminal reports whether f is a real terminal; /dev/null and /dev/zero are not.
func IsTerminal(f *os.File) bool { return IsTerminalFD(int(f.Fd())) }

// Preserve snapshots stdin's termios; the returned func restores it and re-shows the cursor (no-op off a tty).
func Preserve() (restore func()) {
	in := preserveIn
	if !IsTerminalFD(in) {
		return func() {}
	}
	var saved syscall.Termios
	if ioctlTermios(in, ioctlGet, &saved) != nil {
		return func() {}
	}
	showCursor := IsTerminalFD(preserveOut)
	return func() {
		_ = ioctlTermios(in, ioctlSet, &saved)
		if showCursor {
			_, _ = os.Stdout.WriteString("\x1b[?25h")
		}
	}
}
