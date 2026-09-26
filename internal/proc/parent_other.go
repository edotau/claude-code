//go:build !linux && !darwin

package proc

// Parent has no portable source outside /proc: 0 (caller falls back to os.Getppid).
func Parent(pid int) int { return 0 }

// Comm has no portable source outside /proc: "".
func Comm(pid int) string { return "" }
