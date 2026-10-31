//go:build windows

package agent

import "os"

func ownerUID(os.FileInfo) (int, bool) { return 0, false }
