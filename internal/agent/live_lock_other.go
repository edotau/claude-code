//go:build !unix

package agent

import "os"

// tryLockFile cannot lock here, so the slot cap is not enforced off unix.
func tryLockFile(*os.File) bool { return true }
