//go:build !linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// runtimeDirPath is $TMPDIR/claude-code-<uid>/copilot on darwin: no tmpfs runtime dir, so the on-disk per-user
// $TMPDIR is a recorded exception (0700, owner-checked, short-lived pid files swept by `clean`); other OSes have none.
func runtimeDirPath() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errNoRuntimeDir
	}
	base := os.Getenv("TMPDIR")
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("%w: TMPDIR is unset", errNoRuntimeDir)
	}
	return filepath.Join(base, "claude-code-"+strconv.Itoa(os.Getuid()), "copilot"), nil
}

// backingCheck is a no-op on darwin (see runtimeDirPath for the on-disk exception) and unsupported elsewhere.
func backingCheck(string) error {
	if runtime.GOOS == "darwin" {
		return nil
	}
	return errNoRuntimeDir
}
