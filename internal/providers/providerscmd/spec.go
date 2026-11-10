package providerscmd

import (
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func profileFlag() helptext.Flag {
	return helptext.Flag{Name: "profile", Values: providers.ProfileNames}
}

// modelPinAliases offers "alias=" stems for --pin; the user types the model id after the "=" themselves.
func modelPinAliases() []string {
	out := make([]string, 0, len(models.AliasNames)+1)
	for _, s := range models.AliasNames {
		out = append(out, s+"=")
	}
	return append(out, "all=")
}

// ModelsSpec is the completion node for the models verb.
func ModelsSpec() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{profileFlag(), helptext.BoolFlag("unpin"), helptext.BoolFlag("live"),
		{Name: "pin", Values: modelPinAliases}, helptext.BoolFlag("dry-run"), helptext.CompletionFormatFlag()}}
}

// SwitchSpec is the completion node for the switch verb.
func SwitchSpec() *helptext.Node {
	return &helptext.Node{Args: []func() []string{providers.ProfileNames}, Flags: []helptext.Flag{helptext.BoolFlag("dry-run")}}
}

// ResolveSpec is the completion node for the resolve verb.
func ResolveSpec() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{profileFlag(), helptext.BoolFlag("proxy")}}
}
