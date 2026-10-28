//go:build windows

package copilotreg

import "os"

func ownerUID(os.FileInfo) (int, bool) { return 0, false }

// pidAlive: no signal-0 probe on windows; every pid counts as alive so sweeping no-ops.
func pidAlive(int) bool { return true }
