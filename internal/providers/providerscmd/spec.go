package providerscmd

import (
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

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
	return &completion.Node{Flags: []completion.Flag{profileFlag(), completion.BoolFlag("unpin"), completion.BoolFlag("live"),
		{Name: "pin", Values: modelPinAliases}, completion.BoolFlag("dry-run"), completion.FormatFlag()}}
}

// SwitchSpec is the completion node for the switch verb.
func SwitchSpec() *completion.Node {
	return &completion.Node{Args: []func() []string{providers.ProfileNames}, Flags: []completion.Flag{completion.BoolFlag("dry-run")}}
}

// ResolveSpec is the completion node for the resolve verb.
func ResolveSpec() *completion.Node {
	return &completion.Node{Flags: []completion.Flag{profileFlag(), completion.BoolFlag("proxy")}}
}
