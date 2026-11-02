package configure

import (
	"github.com/edotau/claude-code/internal/secretstore"
)

// HasAdminCredential reports, by name only, whether a tailscale admin API credential is set.
func HasAdminCredential() bool {
	if _, _, ok := secretstore.Get("TAILSCALE_OAUTH_CLIENT_ID"); ok {
		return true
	}
	if _, _, ok := secretstore.Get("TAILSCALE_OAUTH_CLIENT_SECRET"); ok {
		return true
	}
	_, _, ok := secretstore.Get("TAILSCALE_API_KEY")
	return ok
}
