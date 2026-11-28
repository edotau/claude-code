package permission

import (
	"strings"

	"github.com/edotau/claude-code/internal/platform/shell"
)

// hasWrite is a raw-segment property: a file redirect, or an unquoted `<` (heredoc, here-string, process substitution).
func hasWrite(seg string) bool {
	if !strings.ContainsAny(seg, "<>") {
		return false
	}
	if shell.FileRedirect(seg) {
		return true
	}
	at := shell.QuoteAt(seg)
	for i := 0; i < len(seg); i++ {
		if seg[i] == '<' && at[i] == 0 {
			return true
		}
	}
	return false
}
