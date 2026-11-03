//go:build windows

package paths

import "os"

// OwnerUID has no uid on Windows, so callers' owner checks refuse.
func OwnerUID(os.FileInfo) (int, bool) { return 0, false }
