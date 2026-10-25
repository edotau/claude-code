package dbximport

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// Candidate is one importable profile: identifying fields and secret PRESENCE only, structurally no secret.
type Candidate struct {
	Name        string
	Host        string // scheme+host only; "" when the source host is unusable
	AuthType    string // the source profile's auth_type, verbatim
	ClientID    string // an identifier, not a secret
	AccountID   string // account_id, an identifier
	WorkspaceID string // workspace_id, an identifier
	HasSecret   bool   // a client_secret or PAT exists in the source file
	Auth        string // row authType (databricks-cli|oauth-m2m|pat), "" when Skip is set
	Skip        string // why the profile cannot become a row
}

// Candidates enumerates the profiles of the config at path.
func Candidates(path string) ([]Candidate, error) {
	ps, err := load(path)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(ps))
	for _, p := range ps {
		out = append(out, classify(p))
	}
	return out, nil
}

func classify(p profile) Candidate {
	c := Candidate{Name: p.name, AuthType: p.authType, ClientID: p.clientID,
		AccountID: p.accountID, WorkspaceID: p.workspaceID,
		HasSecret: p.clientSecret != "" || p.token != ""}
	host, ok := normalizeHost(p.host)
	c.Host = host
	switch {
	case !ok:
		c.Skip = "no usable https host"
	case p.authType == providers.AuthOAuthM2M || (p.clientID != "" && p.clientSecret != ""):
		if p.clientID == "" {
			c.Skip = "oauth-m2m profile without client_id"
		} else {
			c.Auth = providers.AuthOAuthM2M
		}
	case p.token != "" && (p.authType == "" || p.authType == providers.AuthPAT):
		c.Auth = providers.AuthPAT
	case p.authType == providers.AuthDatabricksCLI || p.authType == "":
		c.Auth = providers.AuthDatabricksCLI
	default:
		c.Skip = fmt.Sprintf("unsupported auth_type %q", p.authType)
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

// ReadSecrets returns the named profile's secrets from path.
func ReadSecrets(path, name string) (Secrets, error) {
	ps, err := load(path)
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
