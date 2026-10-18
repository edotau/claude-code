package tailscale

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/secrets"
)

// Doctor is one launch-style check line: CLI found/missing, backend state + node, exit-node state,
// and whether an admin API credential is present (name only, never its value). Never blocks on the
// network for more than 4s.
func Doctor(ctx context.Context) (ok bool, detail string) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	note := credentialNote()
	st, err := Status(ctx)
	switch {
	case errors.Is(err, ErrNoCLI):
		return false, "cli not found on PATH" + note
	case errors.Is(err, ErrNotRunning):
		return false, fmt.Sprintf("backend %s (not connected)%s", st.BackendState, note)
	case err != nil:
		return false, "status: " + err.Error() + note
	}
	exit := "exit-node off"
	switch {
	case st.ExitNodeApproved:
		exit = "exit-node approved"
	case st.ExitNodeOption:
		exit = "exit-node advertised, not approved"
	}
	return true, fmt.Sprintf("%s, node %s, %s%s", st.BackendState, st.DNSName, exit, note)
}

// credentialNote reports whether an admin API credential is configured, by name only.
func credentialNote() string {
	if _, _, ok := secrets.Get("TAILSCALE_OAUTH_CLIENT_ID"); ok {
		return "; api credential TAILSCALE_OAUTH_CLIENT_ID"
	}
	if _, _, ok := secrets.Get("TAILSCALE_API_KEY"); ok {
		return "; api credential TAILSCALE_API_KEY"
	}
	return "; no admin credential"
}
