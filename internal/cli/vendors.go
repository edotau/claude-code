package cli

import (
	// Each vendor CLI registers its roster row and driver from its package init; the program links them all here.
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/gemini"
)
