package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
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

// tpmLimited reports a Databricks 429 body naming the workspace's per-minute token budget for the model.
func tpmLimited(status int, body []byte) bool {
	if status != http.StatusTooManyRequests {
		return false
	}
	var e struct {
		Code    string `json:"error_code"`
		Message string `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return cmp.Or(e.Code, e.Error.Code) == "REQUEST_LIMIT_EXCEEDED" &&
		strings.Contains(cmp.Or(e.Message, e.Error.Message), "per minute")
}

// coolModel holds t's (workspace, model) pair for resp's Retry-After (30 s default, 5 min cap) in r.cool.
func (r *Router) coolModel(t providers.Target, resp *http.Response) {
	now := r.now()
	until := now.Add(retryAfter(resp.Header, now))
	r.mu.Lock()
	r.cool[deadKey(t)] = until
	r.mu.Unlock()
}

func (r *Router) modelCooling(t providers.Target) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now().Before(r.cool[deadKey(t)])
}

// hopBlocked reports a hop target that is cooling, quota-dead for its model, auth-marked or without a credential.
func (r *Router) hopBlocked(state router.State, t providers.Target) bool {
	if _, marked := state.AuthMarked(router.AccountOf(t.Provider), r.now()); marked {
		return true
	}
	return r.modelCooling(t) || r.isDead(t) || !r.credentialPresent(t.Provider)
}

// tpmHop is the next usable workspace after cur in vault order that ok accepts; it may leave a session's profile scope.
func (r *Router) tpmHop(cur providers.Target, ok func(providers.Target) bool) (providers.Target, bool) {
	reg, err := r.registry()
	if err != nil || cur.Provider.Preset != providers.PresetDatabricks || cur.Model == "" {
		return providers.Target{}, false
	}
	state, _ := r.routeStateAt()
	names := reg.ProfilesOf(cur.Provider.Provider)
	start := slices.Index(names, cur.Provider.Name)
	for k := range names {
		p := reg.Providers[names[(start+1+k)%len(names)]]
		if p == nil || p.Name == cur.Provider.Name || p.Preset != providers.PresetDatabricks {
			continue
		}
		t := providers.Target{Provider: p, Model: cur.Model, ContextTokens: cur.ContextTokens}
		if r.hopBlocked(state, t) || !ok(t) {
			continue
		}
		return t, true
	}
	return providers.Target{}, false
}

// logTPMHop writes the one proxy.log line naming a per-minute-token hop, e.g. "dev→qa (429 TPM)".
func (r *Router) logTPMHop(from, to *providers.Provider, model, why string) {
	fmt.Fprintf(r.log, "proxy: %s→%s %s (%s)\n", from.Name, to.Name, agentToken(model), why)
}

// tpmSpareKey carries walkAttempts' *tpmSpareSlot for the ladder's last row into failover.
type tpmSpareKey struct{}

// tpmSpareSlot finds the last row's per-minute-token hop lazily: only a TPM 429 pays for the lookup.
type tpmSpareSlot struct {
	find     func() (providers.Target, bool)
	t        providers.Target
	ok, done bool
}

// found is the hop when failover looked one up and it exists; a nil slot or no lookup is false.
func (s *tpmSpareSlot) found() (providers.Target, bool) {
	if s == nil || !s.done {
		return providers.Target{}, false
	}
	return s.t, s.ok
}

// tpmSpare reports whether in's last ladder row has a workspace to hop to after a TPM 429.
func tpmSpare(in *http.Request) bool {
	if in == nil {
		return false
	}
	s, _ := in.Context().Value(tpmSpareKey{}).(*tpmSpareSlot)
	if s == nil {
		return false
	}
	if !s.done {
		s.t, s.ok = s.find()
		s.done = true
	}
	return s.ok
}

// lacks accepts a hop target the ladder does not already hold whose fresh listing, if any, has the model ready.
func (l attemptLadder) lacks(now time.Time) func(providers.Target) bool {
	return func(t providers.Target) bool {
		return !slices.ContainsFunc(l.rows, func(row providers.Target) bool { return row.Provider.Name == t.Provider.Name }) &&
			!servedUnready(t.Provider, t.Model, now)
	}
}

// tpmFront starts a ladder whose first row holds a per-minute-token cool on its model on the next workspace instead.
func (r *Router) tpmFront(l *attemptLadder) bool {
	if len(l.rows) == 0 || !r.modelCooling(l.rows[0]) {
		return false
	}
	next, ok := r.tpmHop(l.rows[0], l.lacks(r.now()))
	if !ok {
		return false
	}
	r.logTPMHop(l.rows[0].Provider, next.Provider, l.rows[0].Model, "TPM cooling")
	l.rows = slices.Insert(l.rows, 0, next)
	l.coolingFrom++
	return true
}
