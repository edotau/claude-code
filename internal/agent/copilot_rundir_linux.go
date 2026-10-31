//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

// runtimeDirPath is $XDG_RUNTIME_DIR/claude-code/copilot, else /run/user/<uid> when that exists (tmpfs).
func runtimeDirPath() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
		if _, err := os.Stat(base); err != nil {
			return "", fmt.Errorf("%w: XDG_RUNTIME_DIR is unset", errNoRuntimeDir)
		}
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("copilot registry: XDG_RUNTIME_DIR %q is not absolute", base)
	}
	return filepath.Join(base, "claude-code", "copilot"), nil
}

// backingCheck requires dir to sit on tmpfs/ramfs so the session token never reaches a disk.
func backingCheck(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("copilot registry: statfs %s: %w", dir, err)
	}
	if t := int64(st.Type); t != tmpfsMagic && t != ramfsMagic {
		return fmt.Errorf("copilot registry: %s is not on tmpfs (fs type %#x)", dir, t)
	}
	return nil
}
