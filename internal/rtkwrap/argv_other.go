//go:build !darwin && !linux

package rtkwrap

// argvOf has no source outside procargs2 or /proc: nil, so a forked child never wraps.
func argvOf(int) []string { return nil }
