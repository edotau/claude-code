package providers

import (
	"slices"

	"github.com/edotau/claude-code/internal/vault"
)

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
