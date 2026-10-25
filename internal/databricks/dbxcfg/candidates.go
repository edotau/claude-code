package dbxcfg

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// CLIAuthU2M is the .databrickscfg auth_type for a browser (U2M) login; rows call it providers.AuthU2M.
const CLIAuthU2M = "databricks-cli"

// Candidate is one importable profile: identifying fields and secret PRESENCE only, structurally no secret.
type Candidate struct {
	Name        string
	Host        string // scheme+host only; "" when the source host is unusable
	AuthType    string // the source profile's auth_type, verbatim
	ClientID    string // an identifier, not a secret
	AccountID   string // account_id, an identifier
	WorkspaceID string // workspace_id, an identifier
	HasSecret   bool   // a client_secret or PAT exists in the source file
	Auth        string // row authType (u2m|oauth-m2m|pat), "" when Skip is set
	Skip        string // why the profile cannot become a row
}

// defaultOnlySkip explains why a file holding only [DEFAULT] imports nothing.
const defaultOnlySkip = "[DEFAULT] is the CLI's fallback, never imported; copy it into a named profile"

// Candidates enumerates the profiles of the config at path; a DEFAULT-only file lists DEFAULT as skipped.
func Candidates(path string) ([]Candidate, error) {
	ps, hasDefault, err := load(path)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(ps))
	for _, p := range ps {
		out = append(out, classify(p))
	}
	if len(out) == 0 && hasDefault {
		out = append(out, Candidate{Name: defaultSection, Skip: defaultOnlySkip})
	}
	return out, nil
}

// classify maps a profile to a row authType; an explicit auth_type wins over inference from the keys present.
func classify(p profile) Candidate {
	c := Candidate{Name: p.name, AuthType: p.authType, ClientID: p.clientID,
		AccountID: p.accountID, WorkspaceID: p.workspaceID,
		HasSecret: p.clientSecret != "" || p.token != ""}
	host, ok := normalizeHost(p.host)
	c.Host = host
	auth := p.authType
	if auth == "" {
		switch {
		case p.clientID != "" && p.clientSecret != "":
			auth = providers.AuthOAuthM2M
		case p.token != "":
			auth = providers.AuthPAT
		default:
			auth = CLIAuthU2M
		}
	}
	switch {
	case !ok:
		c.Skip = "no usable https host"
	case auth == CLIAuthU2M:
		c.Auth = providers.AuthU2M
	case auth == providers.AuthOAuthM2M && p.clientID == "":
		c.Skip = "oauth-m2m profile without client_id"
	case auth == providers.AuthOAuthM2M, auth == providers.AuthPAT:
		c.Auth = auth
	default:
		c.Skip = fmt.Sprintf("unsupported auth_type %q", p.authType)
	}
	if c.Auth != providers.AuthOAuthM2M && c.Auth != providers.AuthPAT {
		c.HasSecret = false // only these two seal a secret from the file
	}
	return c
}

// normalizeHost reduces a workspace host to https://host; a bare host gets the scheme.
func normalizeHost(h string) (string, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", false
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	return "https://" + u.Host, true
}

// Secrets are the credential values of one profile; read only on an explicit --seal-secrets.
type Secrets struct{ ClientSecret, Token string }

// ReadSecrets returns the named profile's secrets from path; DEFAULT and __settings__ are never readable.
func ReadSecrets(path, name string) (Secrets, error) {
	ps, _, err := load(path)
	if err != nil {
		return Secrets{}, err
	}
	for _, p := range ps {
		if p.name == name {
			return Secrets{ClientSecret: p.clientSecret, Token: p.token}, nil
		}
	}
	return Secrets{}, fmt.Errorf("profile %q not found", name)
}
