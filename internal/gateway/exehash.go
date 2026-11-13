package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

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
