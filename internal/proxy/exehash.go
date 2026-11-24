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

// ExeHash is the sha256 of this process's executable, "" when it cannot be read.
func ExeHash() string {
	exe, err := paths.SelfBinary()
	if err != nil {
		return ""
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

// cachedExeHash is ExeHash memoized on disk by the binary's path, size and mtime, so a status line can afford it.
func cachedExeHash() string {
	exe, err := paths.SelfBinary()
	if err != nil {
		return ""
	}
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
	h := ExeHash()
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
