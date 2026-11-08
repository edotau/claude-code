package usage

import "github.com/edotau/claude-code/internal/completion"

// Spec is the completion node for the usage verb.
func Spec() *completion.Node {
	flag, boolFlag := completion.Plain, completion.BoolFlag
	return &completion.Node{Flags: []completion.Flag{flag("session"), flag("days"), completion.FormatFlag(), boolFlag("workflows"), boolFlag("providers"), flag("show")}}
}
