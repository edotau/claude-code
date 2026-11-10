package server

import (
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// demotion walks t's provider down the slot ladder to the first model not known to be quota-dead.
func (r *Router) demotion(t providers.Target) (providers.Target, bool) {
	seen := map[string]bool{t.Model: true} // a slot table like opus=A sonnet=B haiku=A demotes A→B→A forever
	d, ok := providers.Demote(t.Provider, t.Model)
	for ; ok && !seen[d.Model]; d, ok = providers.Demote(t.Provider, d.Model) {
		seen[d.Model] = true
		if !r.isDead(d) {
			return d, true
		}
	}
	return providers.Target{}, false
}

// credentialPresent caches only present credentials for credTTL, so newly added credentials are seen immediately.
func (r *Router) credentialPresent(p *providers.Provider) bool {
	now := r.now()
	r.mu.Lock()
	if until, ok := r.cred[p.Name]; ok && now.Before(until) {
		r.mu.Unlock()
		return true
	}
	startGen := r.credGen[p.Name]
	r.mu.Unlock()
	if !providers.CredentialPresent(p) {
		return false
	}
	r.storeCredentialPresent(p.Name, startGen, now.Add(credTTL))
	return true
}

// storeCredentialPresent records a successful probe only if no invalidation advanced its generation.
func (r *Router) storeCredentialPresent(name string, startGen int, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.credGen[name] != startGen {
		return
	}
	r.cred[name] = until
}

// invalidateCredential clears cached presence and advances the generation to reject stale probes.
func (r *Router) invalidateCredential(name string) {
	r.mu.Lock()
	delete(r.cred, name)
	r.credGen[name]++
	r.mu.Unlock()
}

// deadKey is profile:canonical-model, so marked ([1m]) and unmarked spellings of one model share a verdict.
func deadKey(t providers.Target) string {
	id := models.Strip1M(t.Model)
	if c, _, ok := models.Resolve(id); ok {
		id = c
	}
	return t.Provider.Name + ":" + id
}

func (r *Router) isDead(t providers.Target) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now().Before(r.dead[deadKey(t)])
}

func (r *Router) markDead(t providers.Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dead[deadKey(t)] = r.now().Add(quotaDeadFor)
}
