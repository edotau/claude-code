package preflight

import (
	"sync"

	"github.com/edotau/claude-code/internal/routing/providers"
)

// credMemo holds one registry's credential probes; each probe is a vault open, so a start asks once per provider.
var credMemo struct {
	mu  sync.Mutex
	reg *providers.Registry
	has map[*providers.Provider]bool
}

// resetCreds drops every memoized probe and binds the memo to reg; Check calls it so each run reads fresh state.
func resetCreds(reg *providers.Registry) {
	credMemo.mu.Lock()
	defer credMemo.mu.Unlock()
	credMemo.reg, credMemo.has = reg, map[*providers.Provider]bool{}
}

// credentialPresent is providers.CredentialPresent, answered once per provider since the last Check on reg.
func credentialPresent(reg *providers.Registry, p *providers.Provider) bool {
	credMemo.mu.Lock()
	defer credMemo.mu.Unlock()
	if credMemo.reg != reg {
		return providers.CredentialPresent(p)
	}
	if ok, hit := credMemo.has[p]; hit {
		return ok
	}
	// Hold the mutex across probe and store so a stale run cannot repopulate after resetCreds.
	ok := providers.CredentialPresent(p)
	credMemo.has[p] = ok
	return ok
}
