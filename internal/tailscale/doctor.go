package tailscale

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/secrets"
)

// Doctor is one launch-style check line: CLI found/missing, backend state + node, exit-node state,
// and whether an admin API credential is present (name only, never its value). Never blocks on the
// network for more than 4s.
func Doctor(ctx context.Context) (ok bool, detail string) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	note, credOK := credentialNote(ctx)
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
	return credOK, fmt.Sprintf("%s, node %s, %s%s", st.BackendState, st.DNSName, exit, note)
}

// CredState is one credential's presence, by name only — never its value.
type CredState struct {
	Name    string
	Source  string
	Present bool
}

// credentialNames is the fixed order Credentials and credentialNote report in.
var credentialNames = secrets.TailscaleNames

// Credentials reports presence and source for every Tailscale credential name, never a value.
func Credentials() []CredState {
	out := make([]CredState, len(credentialNames))
	for i, name := range credentialNames {
		_, source, ok := secrets.Get(name)
		out[i] = CredState{Name: name, Source: source, Present: ok}
	}
	return out
}

// credentialNote reports whether an admin API credential is configured and, when well-formed,
// mints a token to confirm it actually works — by name only, never a value.
func credentialNote(ctx context.Context) (note string, ok bool) {
	cs := Credentials()
	id, sec, api := cs[0].Present, cs[1].Present, cs[2].Present
	switch {
	case id && sec:
		idVal, _, _ := secrets.Get("TAILSCALE_OAUTH_CLIENT_ID")
		secVal, _, _ := secrets.Get("TAILSCALE_OAUTH_CLIENT_SECRET")
		if err := ValidCredential("TAILSCALE_OAUTH_CLIENT_ID", idVal); err != nil {
			return fmt.Sprintf("; TAILSCALE_OAUTH_CLIENT_ID malformed: %v", err), false
		}
		if err := ValidCredential("TAILSCALE_OAUTH_CLIENT_SECRET", secVal); err != nil {
			return fmt.Sprintf("; TAILSCALE_OAUTH_CLIENT_SECRET malformed: %v", err), false
		}
		if _, err := oauthToken(apiBaseURL, idVal, secVal, http.DefaultClient)(ctx); err != nil {
			reason := strings.TrimPrefix(err.Error(), "tailscale: oauth token mint: ")
			return fmt.Sprintf("; api credential TAILSCALE_OAUTH_CLIENT_ID+SECRET rejected (%s)", reason), false
		}
		return "; api credential TAILSCALE_OAUTH_CLIENT_ID+SECRET verified", true
	case id != sec && !api:
		return "; api credential incomplete: need both TAILSCALE_OAUTH_CLIENT_ID and _SECRET", false
	}
	if api {
		keyVal, _, _ := secrets.Get("TAILSCALE_API_KEY")
		if err := ValidCredential("TAILSCALE_API_KEY", keyVal); err != nil {
			return fmt.Sprintf("; TAILSCALE_API_KEY malformed: %v", err), false
		}
		return "; api credential TAILSCALE_API_KEY", true
	}
	return "; no admin credential", true
}
