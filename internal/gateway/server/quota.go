package server

import (
	"cmp"
	"errors"
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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
	return t.Provider.Name + ":" + router.ModelKey(t.Model)
}

// isDead reports a live mark from this process or quota.json; a persisted mark cleared since (a login) lifts ours.
func (r *Router) isDead(t providers.Target) bool {
	now, key := r.now(), deadKey(t)
	state, at := r.routeStateAt()
	persisted := state.ModelDead(router.AccountOf(t.Provider), t.Model, now)
	r.mu.Lock()
	defer r.mu.Unlock()
	if saved, ok := r.deadAt[key]; ok && !persisted && at.After(saved) {
		delete(r.dead, key)
		delete(r.deadAt, key)
	}
	return persisted || now.Before(r.dead[key])
}

// markDead records the denial in memory and persists it off the request path; a failed write is only logged.
func (r *Router) markDead(t providers.Target) {
	now := r.now()
	until := now.Add(quotaDeadFor)
	key := deadKey(t)
	r.mu.Lock()
	r.dead[key] = until
	delete(r.deadAt, key) // a re-mark is unpersisted again until its own write lands
	r.mu.Unlock()
	r.bg.Go(func() {
		ran, err := router.MarkModelDead(router.AccountOf(t.Provider), t.Model, until, now)
		if err != nil || !ran {
			fmt.Fprintf(r.log, "proxy: persist %s %s quota-dead: %v\n", t.Provider.Name, t.Model,
				cmp.Or(err, errors.New("state lock busy")))
			return
		}
		r.mu.Lock()
		if r.dead[key].Equal(until) {
			r.deadAt[key] = r.now()
		}
		r.mu.Unlock()
	})
}
