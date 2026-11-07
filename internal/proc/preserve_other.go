//go:build !linux && !darwin

package proc

// Preserve is a no-op off unix: there is no termios state to put back.
func Preserve() func() {
	return func() {}
}
