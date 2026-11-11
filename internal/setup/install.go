package setup

import (
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// selfBinary is the running harness binary; a seam so install tests never link the test binary.
var selfBinary = paths.SelfBinary

// userBinDir is where install links claude-code onto PATH (~/.local/bin is on PATH by convention).
var userBinDir = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "" // caller's MkdirAll("") fails loudly rather than link under a bogus root path
	}
	return filepath.Join(home, ".local", "bin")
}
