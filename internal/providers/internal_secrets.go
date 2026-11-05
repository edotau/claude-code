package providers

import (
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/vault"
)

// RouterSecretName is the vault record of the loopback router's client secret.
const RouterSecretName = "CLAUDE_CODE_ROUTER_SECRET"

// InternalSecretName reports whether a vault name belongs to the harness itself and may never be handed to a child.
func InternalSecretName(name string) bool {
	return name == RouterSecretName || strings.HasPrefix(strings.ToUpper(name), "HARNESS_")
}

// loginSecrets are the agent logins a launch hands only to its own agent (opencode.OpencodeAPIKeyEnv included).
var loginSecrets = []string{OAuthTokenEnv, CopilotTokenEnv, "OPENCODE_API_KEY"}

// RouteSecret reports a route credential: an agent login, a profile's auth env or any-method secret, any *_OAUTH/*_M2M.
func (r *Registry) RouteSecret(name string) bool {
	if slices.Contains(loginSecrets, name) ||
		strings.HasSuffix(name, "_"+strings.ToUpper(AuthOAuth)) || strings.HasSuffix(name, "_"+strings.ToUpper(AuthM2M)) {
		return true
	}
	for _, p := range r.Providers {
		if p == nil {
			continue
		}
		if p.Auth.Env == name {
			return true
		}
		for _, m := range []string{AuthOAuth, AuthM2M, AuthPAT, AuthKey} {
			if SecretName(p.provider(), p.Name, m) == name {
				return true
			}
		}
	}
	return false
}

// RouteSecretGuard is Registry.RouteSecret over one lazy Load; an unreadable registry refuses every name (fail closed).
func RouteSecretGuard() func(string) bool {
	var reg *Registry
	var err error
	return func(name string) bool {
		if reg == nil && err == nil {
			reg, err = Load()
		}
		return err != nil || reg.RouteSecret(name)
	}
}

// SecretNames unions every env var the harness itself reads (each enabled provider's auth.env, the
// setup-token) with the names index `vault set` has already written — never a hand copy of either.
func SecretNames() []string {
	var out []string
	if reg, err := Load(); err == nil {
		for _, n := range reg.Names() {
			if env := reg.Providers[n].Auth.Env; env != "" {
				out = append(out, env)
			}
		}
	}
	out = append(out, OAuthTokenEnv)
	out = append(out, vault.Names()...)
	slices.Sort(out)
	return slices.Compact(out)
}
