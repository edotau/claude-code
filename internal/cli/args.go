package cli

import "github.com/edotau/claude-code/internal/ui/helptext"

// extractBool delegates to helptext.ExtractBool; providers.go still calls it until that file adopts the helper.
func extractBool(args []string, name string) (bool, []string) {
	return helptext.ExtractBool(args, name)
}
