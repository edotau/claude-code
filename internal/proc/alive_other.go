//go:build !unix

package proc

// Alive is best-effort off unix: no cheap portable probe, so report not-running rather than guess.
func Alive(pid int) bool { return false }
