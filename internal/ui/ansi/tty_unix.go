//go:build linux || darwin

package ansi

import (
	"os"
	"syscall"
	"unsafe"
)

// isTTY reports whether f accepts the termios get ioctl, which only tty devices do (not /dev/null, /dev/zero, files).
func isTTY(f *os.File) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ttyGetReq), uintptr(unsafe.Pointer(&t)))
	return e == 0
}
