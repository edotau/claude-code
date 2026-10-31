//go:build !unix

package agent

import "fmt"

// interrupt is unsupported on this platform.
func interrupt(pid int) error {
	return fmt.Errorf("cancelling a job by pid is unsupported on this platform")
}

// kill is unsupported on this platform.
func kill(pid int) error {
	return fmt.Errorf("killing a job by pid is unsupported on this platform")
}
