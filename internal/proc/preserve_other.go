//go:build !linux && !darwin

package proc

import "os"

// IsTerminalFD is false off unix: there is no termios ioctl to ask.
func IsTerminalFD(int) bool { return false }

// IsTerminal falls back to a char-device check that excludes /dev/null, since no termios ioctl exists here.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

// Preserve is a no-op off unix: there is no termios state to put back.
func Preserve() func() {
	return func() {}
}
