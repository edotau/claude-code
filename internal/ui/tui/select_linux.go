package tui

import "syscall"

// sysSelect is select(2) for reads only; Linux also returns a count, unused since the set is checked.
func sysSelect(nfd int, r *syscall.FdSet, tv *syscall.Timeval) error {
	_, err := syscall.Select(nfd, r, nil, nil, tv)
	return err
}
