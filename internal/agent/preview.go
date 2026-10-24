package agent

import (
	"strings"

	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/textutil"
)

// Preview collapses prompt's whitespace, redacts credential-shaped words, and truncates to n runes.
func Preview(prompt string, n int) string {
	preview := strings.Join(strings.Fields(prompt), " ")
	for w := range strings.FieldsSeq(preview) {
		if shapes.Looks(w) {
			preview = strings.ReplaceAll(preview, w, "REDACTED")
		}
	}
	return textutil.Truncate(preview, n)
}
