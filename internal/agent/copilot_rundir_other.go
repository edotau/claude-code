//go:build !linux

package agent

import (
	"runtime"
)

// backingCheck is a no-op on darwin (paths.CopilotRuntimeDirPath names the on-disk exception) and unsupported elsewhere.
func backingCheck(string) error {
	if runtime.GOOS == "darwin" {
		return nil
	}
	return errNoRuntimeDir
}
