package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// ExeHash is the sha256 of this process's executable (or path when given), "" when it cannot be read.
func ExeHash(path ...string) string {
	exe := ""
	if len(path) > 0 {
		exe = path[0]
	} else if self, err := paths.SelfBinary(); err == nil {
		exe = self
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cachedExeHash is the target proxy executable's sha256 memoized on disk by path, size and mtime.
func cachedExeHash() string {
	exe, err := paths.SelfBinary()
	if err != nil {
		return ""
	}
	exe = sharedProxyExe(exe)
	fi, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	key := fmt.Sprintf("%s %d %d ", exe, fi.Size(), fi.ModTime().UnixNano())
	file := filepath.Join(paths.StateDir(), "proxy.exehash")
	if b, err := os.ReadFile(file); err == nil {
		if h, ok := strings.CutPrefix(string(b), key); ok && len(h) == sha256.Size*2 {
			return h
		}
	}
	h := ExeHash(exe)
	if h != "" {
		_ = paths.AtomicWrite(file, []byte(key+h), 0o600) // best effort: a miss only rehashes
	}
	return h
}

// Stale is the one staleness test (install, doctor, watchdog): st runs another binary; an unreadable self never is.
func Stale(st State) bool {
	self := selfHash()
	return self != "" && st.Exe != self
}
