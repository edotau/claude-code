package shell

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// RTKBinaryEnv overrides the rtk binary; tests cannot rely on rtk being installed, and a wrapper or pinned path also works.
const RTKBinaryEnv = "HARNESS_RTK_BIN"

// RTKBinary returns the absolute rtk binary to invoke, or "" when it is not available. Absence is the
// normal case on a machine without rtk and must degrade to a no-op, never an error.
func RTKBinary() string {
	candidates := []string{RTKPinned(), "rtk"} // the pinned install first, then PATH
	if name := os.Getenv(RTKBinaryEnv); name != "" {
		candidates = []string{name}
	}
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			if abs, err := filepath.Abs(path); err == nil {
				return abs
			}
			return path
		}
	}
	return ""
}

// RTKPinned is the harness-pinned rtk install, BinDir/rtk; it may not exist.
func RTKPinned() string { return filepath.Join(paths.BinDir(), "rtk") }

// RTKOnPath is PATH's rtk ("" when none) and whether it resolves, symlinks followed, to the same file as bin.
func RTKOnPath(bin string) (found string, same bool) {
	found, err := exec.LookPath("rtk")
	if err != nil {
		return "", false
	}
	got, err := filepath.EvalSymlinks(found)
	if err != nil {
		return found, false
	}
	want, err := filepath.EvalSymlinks(bin)
	return found, err == nil && got == want
}
