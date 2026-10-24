package agent

import (
	"strings"

	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/textutil"
)

// Redact replaces credential-shaped words in s with REDACTED, leaving the rest untouched.
func Redact(s string) string {
	for w := range strings.FieldsSeq(s) {
		if shapes.Looks(w) {
			s = strings.ReplaceAll(s, w, "REDACTED")
		}
	}
	return s
}

// Preview collapses prompt's whitespace, redacts credential-shaped words, and truncates to n runes.
func Preview(prompt string, n int) string {
	return textutil.Truncate(Redact(strings.Join(strings.Fields(prompt), " ")), n)
}
