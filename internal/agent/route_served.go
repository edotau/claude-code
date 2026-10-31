package agent

import (
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// unservedTTL is how long a model whose endpoint a live call found gone stays out of served-aware picks.
const unservedTTL = time.Hour

// servedListing is profile's fresh cached listing; false when the switch is off, absent or past TTL. A var for tests.
var servedListing = func(profile string, now time.Time) (Served, bool) {
	if !providers.ServedRoutingOn() {
		return Served{}, false
	}
	s, ok := LoadServed(profile)
	return s, ok && s.Fresh(now) && len(s.Models) > 0 // an empty listing proves nothing: read it as cold
}

// servedKey is the spelling a model is marked and looked up under: the one the agents request from p.
func servedKey(p *providers.Provider, model string) string {
	return p.WireModel(models.Strip1M(strings.TrimSpace(model)))
}

// unservedMarked reports whether wire carries an unexpired mark in the profile account's entry e.
func unservedMarked(e router.Entry, p *providers.Provider, model string, now time.Time) bool {
	at, ok := e.Unserved[servedKey(p, model)]
	return ok && now.Sub(at) < unservedTTL
}

// servedView reads p's listing and unserved marks once into a READY test; fresh is false when no fresh listing exists.
func servedView(p *providers.Provider, now time.Time) (ready func(model string) bool, fresh bool) {
	s, ok := servedListing(p.Name, now)
	if !ok {
		return nil, false
	}
	e := router.LoadState()[router.ProviderAccount(p.Name)]
	return func(model string) bool {
		_, ready := s.Has(p, model)
		return ready && !unservedMarked(e, p, model, now)
	}, true
}

// ServedReady reports whether p's fresh listing marks model READY; fresh=false means keep static behavior.
func ServedReady(p *providers.Provider, model string, now time.Time) (ready, fresh bool) {
	isReady, fresh := servedView(p, now)
	if !fresh {
		return false, false
	}
	return isReady(model), true
}

// ServedFirst is the first READY menu entry respelled for p, Responses-only for codex; "" with fresh=true is none.
func ServedFirst(p *providers.Provider, responsesOnly bool, now time.Time) (model string, fresh bool) {
	isReady, fresh := servedView(p, now)
	if !fresh {
		return "", false
	}
	for _, m := range providers.OpencodeMenu() {
		wire := p.WireModel(m.ID)
		if wire != "" && (!responsesOnly || m.Responses) && isReady(m.ID) {
			return wire, true
		}
	}
	return "", true
}

// MarkUnserved remembers that p's endpoint for model is gone, so served-aware picks skip it for unservedTTL.
func MarkUnserved(p *providers.Provider, model string) error {
	now := time.Now()
	return router.Update(func(s router.State) {
		account := router.ProviderAccount(p.Name)
		e := s[account]
		next := make(map[string]time.Time, len(e.Unserved)+1)
		for k, at := range e.Unserved {
			if now.Sub(at) < unservedTTL {
				next[k] = at
			}
		}
		next[servedKey(p, model)] = now
		e.Unserved = next
		s[account] = e
	})
}
