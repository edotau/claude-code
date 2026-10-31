//go:build darwin

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// runtimeDirPath is $TMPDIR/claude-code-<uid>/copilot: macOS has no tmpfs runtime dir, so the on-disk per-user
// $TMPDIR is a recorded exception (0700, owner-checked, short-lived pid files swept by `clean`).
func runtimeDirPath() (string, error) {
	base := os.Getenv("TMPDIR")
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("%w: TMPDIR is unset", errNoRuntimeDir)
	}
	return filepath.Join(base, "claude-code-"+strconv.Itoa(os.Getuid()), "copilot"), nil
}

// backingCheck is a no-op on darwin: see runtimeDirPath for the documented on-disk exception.
func backingCheck(string) error { return nil }
