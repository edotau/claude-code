package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// HookBinaryName is the hooks-only binary installed beside claude-code.
const HookBinaryName = ".hookd"

// StampFile beside both binaries holds the BuildStamp they were built with.
const StampFile = ".build-stamp"

// BuildStamp is set by -ldflags -X; empty in a dev build.
var BuildStamp string

// StampLine is the .build-stamp content for main: a replaced claude-code changes its size or time.
func StampLine(main os.FileInfo) string {
	return fmt.Sprintf("%s %d %d", BuildStamp, main.Size(), main.ModTime().Unix())
}

// HookBinary is the .hookd sibling of self when it is an executable file, else self.
func HookBinary(self string) string {
	p := filepath.Join(filepath.Dir(self), HookBinaryName)
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && (runtime.GOOS == "windows" || fi.Mode()&0o111 != 0) {
		return p
	}
	return self
}
