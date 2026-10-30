package launch

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// helperBin prefers the installed binary: the overlay file is shared, so a scratch build must never be baked in.
func helperBin(self string) string {
	if bin := filepath.Join(paths.BinDir(), "claude-code"); paths.IsFile(bin) {
		return bin
	}
	return self
}
