// authstate.go — the typed sign-in status every agent and provider row reports.
package providers

import (
	"cmp"
	"encoding/json"
	"os"
	"regexp"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// AuthState is one credential's sign-in as status rows render it, for an agent CLI or a provider row.
type AuthState struct {
	Present  bool      // a credential is on hand (not validated)
	Account  string    // the identity it signs in as ("octocat"); "" when unknown offline
	Source   string    // where it comes from: "ChatGPT login (codex login)", "gh (active)", "COPILOT_GITHUB_TOKEN (stored)"
	Method   string    // AuthCLI, AuthPAT, AuthKey, AuthOAuth or AuthNone
	PlanHint string    // the raw plan the offline probe reports ("max"); "" when none
	Expires  time.Time // token expiry; zero when none is known
	Err      error     // the local probe failed (spawn, timeout, unreadable file): unknown, never "logged out"
}

// Label renders "account via source", or the source alone when the account is unknown.
func (a AuthState) Label() string {
	if a.Account == "" {
		return a.Source
	}
	return a.Account + " via " + a.Source
}

// Subscription is the plan a sign-in holds: Raw feeds agent_plans matching, Name the "(Max 20x)" label.
type Subscription struct {
	Raw, Name string
	Account   string // the identity a network lookup named when AuthState.Account was empty
}

var maxTierRE = regexp.MustCompile(`max_(\d+x)`)

// MaxTierName names a rate-limit tier ("default_claude_max_20x"): 20x is "Ultra", 5x "Max 5x"; "" when not a Max tier.
func MaxTierName(tier string) string {
	m := maxTierRE.FindStringSubmatch(tier)
	switch {
	case m == nil:
		return ""
	case m[1] == "20x":
		return "Ultra"
	}
	return "Max " + m[1]
}

// ClaudeOAuthAccount is the oauthAccount block of ~/.claude.json.
type ClaudeOAuthAccount struct {
	Org      string `json:"organizationRateLimitTier"`
	User     string `json:"userRateLimitTier"`
	PlanName string `json:"planDisplayName"`
}

// Tier is the raw rate-limit tier ("default_claude_max_20x"), user before org.
func (a ClaudeOAuthAccount) Tier() string { return cmp.Or(a.User, a.Org) }

// ClaudeAccount reads ~/.claude.json's oauthAccount; zero when the file is absent or unreadable.
func ClaudeAccount() ClaudeOAuthAccount {
	b, err := os.ReadFile(paths.ClaudeJSON())
	if err != nil {
		return ClaudeOAuthAccount{}
	}
	var cfg struct {
		Account ClaudeOAuthAccount `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ClaudeOAuthAccount{}
	}
	return cfg.Account
}

// ProviderStatus is one provider's credential state: which source would serve it today, never the
// value itself.
type ProviderStatus struct {
	Source string // env | vault | auth.file | command | cli | none
	Note   string
}

// Resolve reports credential presence in env -> vault -> auth.file -> command order, without resolving a key.
func Resolve(p *Provider) ProviderStatus {
	if st, ok := credentialFreeStatus(p); ok {
		return st
	}
	var st ProviderStatus
	if p.Auth.Env != "" {
		source, err := vault.HasSecretErr(p.Auth.Env)
		st.Source = source
		if err != nil {
			st.Note = "vault unreadable: " + err.Error()
		}
	}
	if st.Source == "" {
		st.Source = profileSecretSource(p, &st)
	}
	if st.Source == "" {
		st.Source = fallbackSource(p)
	}
	if st.Source == "none" && p.Auth.Env != "" && st.Note == "" {
		st.Note = "set " + p.Auth.Env
	}
	return st
}

// credentialFreeStatus is the status of a row that holds no credential of its own (none, cli); ok false otherwise.
func credentialFreeStatus(p *Provider) (ProviderStatus, bool) {
	switch p.Auth.Type {
	case AuthNone:
		return ProviderStatus{Source: "none", Note: "no credential needed"}, true
	case AuthCLI:
		st := ProviderStatus{Source: "cli"}
		if p.Auth.Login != "" {
			st.Note = "via `" + p.Auth.Login + "` login"
		}
		return st, true
	}
	return ProviderStatus{}, false
}

// profileSecretSource is the source of the profile's own sealed or key secret ("" when none).
func profileSecretSource(p *Provider, st *ProviderStatus) string {
	name := cmp.Or(AuthSecretName(p), keySecretName(p))
	if name == "" {
		return ""
	}
	source, err := vault.HasSecretErr(name)
	if err != nil && st.Note == "" {
		st.Note = "vault unreadable: " + err.Error()
	}
	return source
}

// fallbackSource names the auth.file or command source when the vault has none, else "none".
func fallbackSource(p *Provider) string {
	switch {
	case fileCredential(p) != "":
		return "auth.file"
	case p.Auth.Command != "":
		return "command"
	}
	return "none"
}

// CredSource names where p's credential resolves from (vault|env|command|auth.file|cli|none|missing).
func CredSource(p *Provider) (source, note string) {
	st := Resolve(p)
	if st.Source == "none" && p.Auth.Method() != AuthNone {
		return "missing", st.Note
	}
	return st.Source, st.Note
}
