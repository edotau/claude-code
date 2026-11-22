package paths

import "path/filepath"

// HookBinaryName is the hooks-only binary installed beside claude-code.
const HookBinaryName = "harness-hook"

// StampFile beside both binaries holds the BuildStamp they were built with.
const StampFile = ".build-stamp"

// BuildStamp is set by -ldflags -X; empty in a dev build.
var BuildStamp string

// HookBinary is the harness-hook sibling of self when it is a file, else self.
func HookBinary(self string) string {
	if p := filepath.Join(filepath.Dir(self), HookBinaryName); IsFile(p) {
		return p
	}
	return self
}
