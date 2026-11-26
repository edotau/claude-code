package tui

import (
	"syscall"
	"unsafe"
)

// sysSelect is pselect6(2) for reads only: arm64 has no select(2), so every linux arch issues the same syscall.
func sysSelect(nfd int, r *syscall.FdSet, tv *syscall.Timeval) error {
	ts := syscall.Timespec{Sec: int64(tv.Sec), Nsec: int64(tv.Usec) * 1000}
	_, _, errno := syscall.Syscall6(syscall.SYS_PSELECT6, uintptr(nfd), uintptr(unsafe.Pointer(r)), 0, 0, uintptr(unsafe.Pointer(&ts)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}
