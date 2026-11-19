package server

import (
	"cmp"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

var persistModelDead = router.MarkModelDead
var probeCredential = providers.CredentialPresent
var credInflight sync.Map // probes in flight, keyed by credProbe
var credMissTTL = 5 * time.Second

type credProbe struct {
	r    *Router
	name string
}

const (
	credPresentKey = "present:"
	credMissKey    = "miss:"
	credStampKey   = "stamp:"
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

func (r *Router) credentialPresent(p *providers.Provider) (present bool) {
	name := p.Name
	now := r.now()
	cached, present, stamp, gen := r.cachedCredential(name, now)
	if cached {
		return present
	}
	k := credProbe{r, name}
	done := make(chan struct{})
	if wait, loaded := credInflight.LoadOrStore(k, done); loaded {
		<-wait.(chan struct{})
		if cached, present, _, _ := r.cachedCredential(name, r.now()); cached {
			return present
		}
		return probeCredential(p)
	}
	defer func() { credInflight.Delete(k); close(done) }()
	if cached, present, _, _ := r.cachedCredential(name, r.now()); cached { // a probe finished since our first read
		return present
	}
	present = probeCredential(p)
	r.storeCredentialProbe(p, gen, stamp, now, present)
	return present
}

func (r *Router) cachedCredential(name string, now time.Time) (cached, present bool, stamp time.Time, gen int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	present = now.Before(r.cred[credPresentKey+name])
	// A vault-backed sign-in changes the stamp that registry() reloads on.
	miss := r.regRetry.IsZero() && r.regStamp.Equal(r.cred[credStampKey+name]) && now.Before(r.cred[credMissKey+name])
	return present || miss, present, r.regStamp, r.credGen[name]
}

func (r *Router) storeCredentialProbe(p *providers.Provider, gen int, stamp, now time.Time, present bool) {
	name := p.Name
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.credGen[name] != gen {
		return
	}
	if present {
		r.cred[credPresentKey+name] = now.Add(credTTL)
	} else if r.regStamp.Equal(stamp) && r.regRetry.IsZero() && (r.reg == nil || r.reg.Providers[name] == p) {
		r.cred[credMissKey+name] = now.Add(credMissTTL)
		r.cred[credStampKey+name] = stamp
	}
}

// The generation prevents an in-flight probe from repopulating an invalidated row.
func (r *Router) invalidateCredential(name string) {
	r.mu.Lock()
	delete(r.cred, credPresentKey+name)
	delete(r.cred, credMissKey+name)
	delete(r.cred, credStampKey+name)
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
	goStateWrite(r, func() {
		ran, err := persistModelDead(router.AccountOf(t.Provider), t.Model, until, now)
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
