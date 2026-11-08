//go:build linux || darwin

package proc

import (
	"syscall"
	"unsafe"
)

// Size is the terminal's (columns, rows) from TIOCGWINSZ.
func Size(fd int) (int, int, error) {
	var ws struct{ Row, Col, X, Y uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws))); e != 0 {
		return 0, 0, e
	}
	return int(ws.Col), int(ws.Row), nil
}
