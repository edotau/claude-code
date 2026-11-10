package vaultcmd

import (
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func vaultSections() []string {
	return []string{"secrets", "pins", "env", "providers", "agents", "settings", "vault", "all"}
}

// Spec is the completion node for the vault verb.
func Spec() *helptext.Node {
	flag, boolFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.CompletionFormatFlag
	secretNames := providers.SecretNames
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"ls": {Args: []func() []string{vaultSections}, Flags: []helptext.Flag{boolFlag("tailscale"), formatFlag()}},
		"set": {Args: []func() []string{secretNames}, Flags: []helptext.Flag{boolFlag("tailscale"), boolFlag("api-key"),
			boolFlag("dry-run")}},
		"get":           {Args: []func() []string{secretNames}, Flags: []helptext.Flag{boolFlag("export"), boolFlag("key")}},
		"delete":        {Args: []func() []string{secretNames}},
		"import":        {Flags: []helptext.Flag{boolFlag("yes")}},
		"export":        {Args: []func() []string{secretNames}, Flags: []helptext.Flag{flag("out")}},
		"edit":          {},
		"rotate-secret": {Flags: []helptext.Flag{boolFlag("force")}},
	}}
}
