// Package buildver reports the binary's build version.
package buildver

import "runtime/debug"

// Version is the module version stamped into the binary, or "devel" when none is.
func Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "devel"
}
