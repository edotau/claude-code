package tailscale

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// Doctor is one check line: CLI, backend and node, exit-node state, credential name; each step waits at most 4s.
func Doctor(ctx context.Context) (ok bool, detail string) {
	credCtx, cancelCred := context.WithTimeout(ctx, 4*time.Second)
	note, credOK := credentialNote(credCtx)
	cancelCred()
	statusCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	st, err := Status(statusCtx)
	switch {
	case errors.Is(err, errNoCLI):
		return false, "cli not found on PATH" + note
	case errors.Is(err, errNotRunning):
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

// CredentialNames are the Tailscale credentials the harness stores, in the order Credentials and credentialNote report.
var CredentialNames = []string{oauthIDName, oauthSecretName, apiKeyName, authKeyName}

// Credentials reports presence and source for every Tailscale credential name, never a value.
func Credentials() []CredState {
	out := make([]CredState, len(CredentialNames))
	for i, name := range CredentialNames {
		_, source, ok := vault.Get(name)
		out[i] = CredState{Name: name, Source: source, Present: ok}
	}
	return out
}

// credentialNote names the configured admin credential and, when well-formed, mints a token to confirm it works.
func credentialNote(ctx context.Context) (note string, ok bool) {
	present := map[string]bool{}
	for _, c := range Credentials() {
		present[c.Name] = c.Present
	}
	id, sec, api := present[oauthIDName], present[oauthSecretName], present[apiKeyName]
	switch {
	case id && sec:
		return oauthNote(ctx)
	case id != sec && !api:
		return "; api credential incomplete: need both TAILSCALE_OAUTH_CLIENT_ID and _SECRET", false
	case api:
		return apiKeyNote()
	}
	return "; no admin credential", true
}

// oauthNote validates the OAuth client pair's shape, then mints a token to confirm it works.
func oauthNote(ctx context.Context) (note string, ok bool) {
	idVal, _, _ := vault.Get(oauthIDName)
	secVal, _, _ := vault.Get(oauthSecretName)
	if err := textutil.ValidCredential(oauthIDName, idVal); err != nil {
		return fmt.Sprintf("; TAILSCALE_OAUTH_CLIENT_ID malformed: %v", err), false
	}
	if err := textutil.ValidCredential(oauthSecretName, secVal); err != nil {
		return fmt.Sprintf("; TAILSCALE_OAUTH_CLIENT_SECRET malformed: %v", err), false
	}
	if _, err := newOAuthClient(idVal, secVal).token(ctx); err != nil {
		reason := strings.TrimPrefix(err.Error(), "tailscale: oauth token mint: ")
		return fmt.Sprintf("; api credential TAILSCALE_OAUTH_CLIENT_ID+SECRET rejected (%s)", reason), false
	}
	return "; api credential TAILSCALE_OAUTH_CLIENT_ID+SECRET verified", true
}

// apiKeyNote validates the API key's shape.
func apiKeyNote() (note string, ok bool) {
	keyVal, _, _ := vault.Get(apiKeyName)
	if err := textutil.ValidCredential(apiKeyName, keyVal); err != nil {
		return fmt.Sprintf("; TAILSCALE_API_KEY malformed: %v", err), false
	}
	return "; api credential TAILSCALE_API_KEY", true
}
