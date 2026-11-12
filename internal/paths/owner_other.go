//go:build !unix && !windows

package paths

import "os"

// OwnerUID has no uid on this platform, so callers' owner checks refuse.
func OwnerUID(os.FileInfo) (int, bool) { return 0, false }
