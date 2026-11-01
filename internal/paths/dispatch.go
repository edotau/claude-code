package paths

import (
	"os"
	"path/filepath"
)

// DispatchDir is the directory holding dispatch records and job logs.
func DispatchDir() string { return filepath.Join(StateDir(), "dispatch") }

// EnsureDispatchDir creates the 0700 dispatch dir and returns its path.
func EnsureDispatchDir() (string, error) {
	dir := DispatchDir()
	return dir, os.MkdirAll(dir, 0o700)
}
