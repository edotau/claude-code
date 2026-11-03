//go:build windows

package agentconfig

import "os"

func ownerUID(os.FileInfo) (int, bool) { return 0, false }
