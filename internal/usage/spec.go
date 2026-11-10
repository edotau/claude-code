package usage

import "github.com/edotau/claude-code/internal/ui/helptext"

// Spec is the completion node for the usage verb.
func Spec() *helptext.Node {
	flag, boolFlag := helptext.Plain, helptext.BoolFlag
	return &helptext.Node{Flags: []helptext.Flag{flag("session"), flag("days"), helptext.CompletionFormatFlag(), boolFlag("workflows"), boolFlag("providers"), flag("show")}}
}
