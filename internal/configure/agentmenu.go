// agentmenu.go — one submenu per agent CLI, all one shape: the two billing modes (Subscription, API key), then the
// actions that refresh or mint the credential (Login, then a token or key mint), then Back.
package configure

import (
	"github.com/edotau/claude-code/internal/providers"
)

// KeyVerified live-probes a keyed provider; false when absent or not accepted.
func KeyVerified(p *providers.Provider) bool {
	if p == nil || !providers.CredentialPresent(p) {
		return false
	}
	status, _, _ := ProbeCached(KeyTarget{Provider: p})
	return status == "verified"
}

// KeyPresent reports whether provider name has a stored key.
func KeyPresent(reg *providers.Registry, provider string) (*providers.Provider, bool) {
	p := ProfileBy(reg, provider, providers.AuthKey)
	return p, p != nil && providers.CredentialPresent(p)
}

// ProfileBy is the first profile under provider signing in by method (key, cli, ...), or nil.
func ProfileBy(reg *providers.Registry, provider, method string) *providers.Provider {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.Auth.Method() == method {
			return p
		}
	}
	return nil
}

// ProfileName is p's name, "" for nil.
func ProfileName(p *providers.Provider) string {
	if p == nil {
		return ""
	}
	return p.Name
}
