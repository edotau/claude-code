// authstate.go — the typed sign-in status every agent and provider row reports.
package providers

import (
	"cmp"
	"encoding/json"
	"os"
	"regexp"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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
