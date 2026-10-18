package memory

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// MaintenancePath is the marker every hook/worker consults before initializing, spawning or writing (plan §4).
func MaintenancePath() string { return filepath.Join(StateDir(), "maintenance") }

// SetMaintenance raises the marker with a human-readable reason (migration apply, store repair).
func SetMaintenance(reason string) error {
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(MaintenancePath(), []byte(reason+"\n"), 0o600)
}

// ClearMaintenance removes the marker; an absent marker is not an error.
func ClearMaintenance() error {
	if err := os.Remove(MaintenancePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// InMaintenance reports whether the marker exists; reason is its trimmed body and may be empty.
func InMaintenance() (reason string, ok bool) {
	data, err := os.ReadFile(MaintenancePath())
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}
