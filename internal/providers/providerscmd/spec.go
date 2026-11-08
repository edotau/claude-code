package providerscmd

import (
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func boolFlag(name string) completion.Flag { return completion.Flag{Name: name, IsBool: true} }

func profileFlag() completion.Flag {
	return completion.Flag{Name: "profile", Values: providers.ProfileNames}
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
func ModelsSpec() *completion.Node {
	format := completion.Flag{Name: "format", Values: func() []string { return []string{helptext.Text, helptext.JSON} }}
	return &completion.Node{Flags: []completion.Flag{profileFlag(), boolFlag("unpin"), boolFlag("live"),
		{Name: "pin", Values: modelPinAliases}, boolFlag("dry-run"), format}}
}

// SwitchSpec is the completion node for the switch verb.
func SwitchSpec() *completion.Node {
	return &completion.Node{Args: []func() []string{providers.ProfileNames}, Flags: []completion.Flag{boolFlag("dry-run")}}
}

// ResolveSpec is the completion node for the resolve verb.
func ResolveSpec() *completion.Node {
	return &completion.Node{Flags: []completion.Flag{profileFlag(), boolFlag("proxy")}}
}
