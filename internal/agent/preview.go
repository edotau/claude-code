package agent

import (
	"strings"

	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/textutil"
)

// Preview collapses prompt's whitespace, redacts credential-shaped words, and truncates to n runes.
func Preview(prompt string, n int) string {
	return textutil.Truncate(shapes.Redact(strings.Join(strings.Fields(prompt), " ")), n)
}
