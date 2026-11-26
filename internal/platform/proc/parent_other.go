//go:build !linux && !darwin

package proc

// Parent has no portable source outside /proc: 0 (caller falls back to os.Getppid).
func Parent(pid int) int { return 0 }

// Comm has no portable source outside /proc: "".
func Comm(pid int) string { return "" }

// Exe has no portable source outside /proc or sysctl: "".
func Exe(pid int) string { return "" }

// Descendants has no portable source outside /proc or ps: nil.
func Descendants(pid int) []int { return nil }
