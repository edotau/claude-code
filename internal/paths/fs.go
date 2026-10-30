package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// IsFile reports whether p exists and is a regular (non-dir) entry.
func IsFile(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }

// IsDir reports whether p exists and is a directory.
func IsDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// FileExists reports whether p exists (any type).
func FileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// SafeName reports whether name is a single safe path component — an unsafe name (`../../escape`,
// `..`, a dot-file, a backslash on Linux) would let a joined path escape the root it is checked under.
func SafeName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		name == filepath.Base(name) &&
		!strings.ContainsAny(name, `/\`) &&
		!strings.HasPrefix(name, ".")
}

// DirSize sums a file's size, or every file under a directory. Best-effort: an unreadable entry counts 0.
func DirSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	if !st.IsDir() {
		return st.Size()
	}
	var total int64
	_ = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree contributes 0, never aborts the walk
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
