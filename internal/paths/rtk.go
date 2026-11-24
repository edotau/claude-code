package paths

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// RTKBinaryEnv overrides the rtk binary: tests cannot rely on rtk being installed, and a wrapper path also works.
const RTKBinaryEnv = "HARNESS_RTK_BIN"

// RTKQuietEnv set to 1 silences rtk's missing-hook banner on every rtk the harness or an agent runs.
const RTKQuietEnv = "RTK_SUPPRESS_HOOK_WARNING"

// RTKTelemetryEnv set to 1 blocks rtk's telemetry ping in every rtk the harness or an agent runs.
const RTKTelemetryEnv = "RTK_TELEMETRY_DISABLED"

// RTKBinary returns the absolute rtk binary to invoke, or "" when it is not available.
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

// RTKConfig is rtk's own config.toml (macOS Application Support, else ~/.config); "" on Windows or with no home.
func RTKConfig() string {
	home, err := os.UserHomeDir()
	switch {
	case runtime.GOOS == "windows" || err != nil:
		return ""
	case runtime.GOOS == "darwin":
		return filepath.Join(home, "Library", "Application Support", "rtk", "config.toml")
	}
	return filepath.Join(home, ".config", "rtk", "config.toml")
}

// RTKPinned is the harness-pinned rtk install, BinDir/rtk; it may not exist.
func RTKPinned() string { return filepath.Join(BinDir(), "rtk") }

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
