//go:build linux

package agent

import (
	"fmt"
	"syscall"
)

const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

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
