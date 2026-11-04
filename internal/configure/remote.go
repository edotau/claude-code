package configure

import (
	"github.com/edotau/claude-code/internal/vault"
)

// HasAdminCredential reports, by name only, whether a tailscale admin API credential is set.
func HasAdminCredential() bool {
	for _, n := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY"} {
		if src, _ := vault.HasSecretErr(n); src != "" { // presence only: no decrypt per home render
			return true
		}
	}
	return false
}
