package shellscope

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
	candidates := []string{filepath.Join(paths.BinDir(), "rtk"), "rtk"} // the pinned install first, then PATH
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
