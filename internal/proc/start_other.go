//go:build !linux && !darwin

package proc

import "errors"

// StartTime has no source on this OS: an error, so callers reject rather than trust a bare pid.
func StartTime(pid int) (string, error) {
	return "", errors.New("proc: start time unsupported on this OS")
}
