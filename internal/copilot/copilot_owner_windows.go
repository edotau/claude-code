//go:build windows

package copilot

import "os"

func ownerUID(os.FileInfo) (int, bool) { return 0, false }
