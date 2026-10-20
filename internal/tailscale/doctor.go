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

// CredState is one credential's presence, by name only — never its value.
type CredState struct {
	Name    string
	Source  string
	Present bool
}

// credentialNames is the fixed order Credentials and credentialNote report in.
var credentialNames = []string{
	"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY", "TAILSCALE_AUTHKEY",
}

// Credentials reports presence and source for every Tailscale credential name, never a value.
func Credentials() []CredState {
	out := make([]CredState, len(credentialNames))
	for i, name := range credentialNames {
		_, source, ok := secrets.Get(name)
		out[i] = CredState{Name: name, Source: source, Present: ok}
	}
	return out
}

// credentialNote reports whether an admin API credential is configured, by name only.
func credentialNote() string {
	cs := Credentials()
	id, sec, api := cs[0].Present, cs[1].Present, cs[2].Present
	switch {
	case id && sec:
		return "; api credential TAILSCALE_OAUTH_CLIENT_ID+SECRET"
	case id != sec && !api:
		return "; api credential incomplete: need both TAILSCALE_OAUTH_CLIENT_ID and _SECRET"
	}
	if api {
		return "; api credential TAILSCALE_API_KEY"
	}
	return "; no admin credential"
}
