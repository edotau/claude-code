package paths

import "path/filepath"

// DispatchDir is the directory holding dispatch records and job logs.
func DispatchDir() string { return filepath.Join(StateDir(), "dispatch") }
