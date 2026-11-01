//go:build !linux

package copilot

import (
	"errors"
	"runtime"
)

var errNoRuntimeDir = errors.New("copilot registry: no private runtime directory on this platform")

// backingCheck is a no-op on darwin (paths.CopilotRuntimeDirPath names the on-disk exception) and unsupported elsewhere.
func backingCheck(string) error {
	if runtime.GOOS == "darwin" {
		return nil
	}
	return errNoRuntimeDir
}
