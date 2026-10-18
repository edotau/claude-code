package configure

import (
	"context"
	"time"

	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tailscale"
)

// tailscaleKeyTarget reports the tailscale row for configure's keys section: present means the
// daemon is logged in (`tailscale status` Running). The admin API credential is a second,
// independent state, noted in the detail but never required for the row to read present.
func tailscaleKeyTarget() keyTarget {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	st, err := tailscale.Status(ctx)
	if err == nil {
		return keyTarget{label: "tailscale", present: true, source: "tailscale login (" + st.DNSName + ")" + tailscaleCredentialNote()}
	}
	return keyTarget{
		label:     "tailscale",
		present:   false,
		loginArgv: []string{"tailscale", "login"},
		envVar:    "TAILSCALE_OAUTH_CLIENT_SECRET",
		fixCmd:    "tailscale login · claude-code secrets set TAILSCALE_OAUTH_CLIENT_SECRET",
	}
}

// tailscaleCredentialNote reports whether an admin API credential is configured, by name only.
func tailscaleCredentialNote() string {
	if _, _, ok := secrets.Get("TAILSCALE_OAUTH_CLIENT_SECRET"); ok {
		return "; admin credential TAILSCALE_OAUTH_CLIENT_SECRET"
	}
	if _, _, ok := secrets.Get("TAILSCALE_API_KEY"); ok {
		return "; admin credential TAILSCALE_API_KEY"
	}
	return "; no admin credential"
}
