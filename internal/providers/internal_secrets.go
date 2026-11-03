package providers

import "strings"

// RouterSecretName is the vault record of the loopback router's client secret.
const RouterSecretName = "CLAUDE_CODE_ROUTER_SECRET"

// InternalSecretName reports whether a vault name belongs to the harness itself and may never be handed to a child.
func InternalSecretName(name string) bool {
	return name == RouterSecretName || strings.HasPrefix(strings.ToUpper(name), "HARNESS_")
}
