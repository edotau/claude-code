package tui

import "syscall"

// sysSelect is select(2) for reads only; darwin's syscall.Select returns no count, so callers read the set.
func sysSelect(nfd int, r *syscall.FdSet, tv *syscall.Timeval) error {
	return syscall.Select(nfd, r, nil, nil, tv)
}
