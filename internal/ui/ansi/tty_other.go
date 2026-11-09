//go:build !linux && !darwin

package ansi

import "os"

// isTTY falls back to a char-device check that excludes /dev/null, since no termios ioctl exists here.
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}
