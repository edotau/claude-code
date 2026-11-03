// auth.go — the optional Authenticator a Driver adds, and the agent-level entry points over it.
package agent

import (
	"context"

	"github.com/edotau/claude-code/internal/providers"
)

// Authenticator is the optional sign-in surface a Driver adds; BaseEngine does not implement it.
type Authenticator interface {
	// AuthStatus is the CLI's own sign-in from local state (files, keyring, a local status command); no network.
	AuthStatus() providers.AuthState
	// Plan is the subscription st's sign-in holds; it may reach the network and caches per account itself.
	Plan(ctx context.Context, st providers.AuthState) providers.Subscription
	// Verify probes the sign-in for the provider sign-in rows; a failed probe is VerdictTransient.
	Verify(ctx context.Context) (providers.AuthVerdict, string)
}

func authenticator(name string) (Authenticator, bool) {
	a, ok := schemaFor(name).eng().(Authenticator)
	return a, ok
}

// AuthStatus is name's own sign-in; the zero state for an agent with no Authenticator. Safe for concurrent use.
func AuthStatus(name string) providers.AuthState {
	if a, ok := authenticator(name); ok {
		return a.AuthStatus()
	}
	return providers.AuthState{}
}

// AuthPlan is name's subscription for st; zero when st is not present or the agent has no Authenticator.
func AuthPlan(ctx context.Context, name string, st providers.AuthState) providers.Subscription {
	if a, ok := authenticator(name); ok && st.Present {
		return a.Plan(ctx, st)
	}
	return providers.Subscription{}
}

// Verify probes name's own CLI sign-in for the provider sign-in rows.
func Verify(ctx context.Context, name string) (providers.AuthVerdict, string) {
	if a, ok := authenticator(name); ok {
		return a.Verify(ctx)
	}
	return providers.VerdictTransient, "unknown sign-in " + name
}
