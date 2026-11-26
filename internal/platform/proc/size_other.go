//go:build !linux && !darwin

package proc

import "errors"

// Size has no terminal query off unix.
func Size(int) (int, int, error) {
	return 0, 0, errors.New("proc: terminal size is not supported on this platform")
}
