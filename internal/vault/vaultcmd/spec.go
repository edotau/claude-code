package vaultcmd

import (
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/providers"
)

func vaultSections() []string {
	return []string{"secrets", "pins", "env", "providers", "agents", "settings", "vault", "all"}
}

// Spec is the completion node for the vault verb.
func Spec() *completion.Node {
	flag, boolFlag, formatFlag := completion.Plain, completion.BoolFlag, completion.FormatFlag
	secretNames := providers.SecretNames
	return &completion.Node{Sub: map[string]*completion.Node{
		"list": {Args: []func() []string{vaultSections}, Flags: []completion.Flag{boolFlag("tailscale"), formatFlag()}},
		"set": {Args: []func() []string{secretNames}, Flags: []completion.Flag{boolFlag("tailscale"), boolFlag("api-key"),
			boolFlag("dry-run")}},
		"get":           {Args: []func() []string{secretNames}, Flags: []completion.Flag{boolFlag("export"), boolFlag("key")}},
		"delete":        {Args: []func() []string{secretNames}},
		"import":        {Flags: []completion.Flag{boolFlag("yes")}},
		"export":        {Args: []func() []string{secretNames}, Flags: []completion.Flag{flag("out")}},
		"edit":          {},
		"rotate-secret": {Flags: []completion.Flag{boolFlag("force")}},
	}}
}
