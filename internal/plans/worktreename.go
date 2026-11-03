package plans

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// WorktreeName is a linked worktree's name (matched against task claims), or "" for the main checkout.
func WorktreeName(top string) string {
	if paths.MainCheckout(top) != top {
		return filepath.Base(top)
	}
	return ""
}
