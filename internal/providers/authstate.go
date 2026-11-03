// authstate.go — the typed sign-in status every agent and provider row reports.
package providers

import "time"

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
