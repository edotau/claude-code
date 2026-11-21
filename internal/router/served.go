package router

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
)

// unservedTTL is how long a model whose endpoint a live call found gone stays out of served-aware picks.
const unservedTTL = time.Hour

// servedListing is profile's fresh cached listing; false when the switch is off, absent or past TTL. A var for tests.
var servedListing = func(profile string, now time.Time) (serving.Served, bool) {
	if !providers.ServedRoutingOn() {
		return serving.Served{}, false
	}
	return serving.FreshShared(profile, now)
}

// servedKey is the spelling a model is marked and looked up under: the one the agents request from p.
func servedKey(p *providers.Provider, model string) string {
	return p.WireModel(models.Strip1M(strings.TrimSpace(model)))
}

// unservedMarked reports whether model carries an unexpired mark in the profile account's entry e.
func unservedMarked(e Entry, p *providers.Provider, model string, now time.Time) bool {
	at, ok := e.Unserved[servedKey(p, model)]
	return ok && now.Sub(at) < unservedTTL
}

// servedView reads p's listing and unserved marks once into a READY test; fresh is false when no fresh listing exists.
func servedView(p *providers.Provider, now time.Time) (ready func(model string) bool, fresh bool) {
	s, ok := servedListing(p.Name, now)
	if !ok {
		return nil, false
	}
	e := sharedState()[ProviderAccount(p.Name)]
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

// ServedListed reads p's cached listing whatever the servedAware switch says; fresh is false when absent or past TTL.
func ServedListed(p *providers.Provider, model string, now time.Time) (listed, ready, fresh bool) {
	s, ok := serving.FreshShared(p.Name, now)
	if !ok {
		return false, false, false
	}
	listed, ready = s.Has(p, model)
	return listed, ready, true
}

// SpellServed is SpellPin, but a bare unlisted id is databricks-<id> where p's fresh listing has that READY.
func SpellServed(p *providers.Provider, id string, now time.Time) string {
	// the [1m] marker never decides the spelling
	id = models.Strip1M(id)
	wire := providers.SpellPin(p, id)
	if p == nil || p.SpellingRule() != "databricks-serving" || wire != id || strings.ContainsAny(id, "/:") ||
		strings.HasPrefix(id, models.DatabricksPrefix) {
		// a catalog id SpellPin already prefixed, or one already qualified
		return wire
	}
	if exactReady(p, models.DatabricksPrefix+id, now) {
		return models.DatabricksPrefix + id
	}
	return wire
}

// exactReady reports p's fresh listing names wire exactly and has it READY.
func exactReady(p *providers.Provider, wire string, now time.Time) bool {
	s, ok := serving.FreshShared(p.Name, now)
	if !ok {
		return false
	}
	for _, m := range s.Models {
		if m.ID == wire {
			return m.IsReady()
		}
	}
	return false
}

// ListedNotReady reports p's fresh listing lacks wire as a READY endpoint (unlisted too); false without a fresh one.
func ListedNotReady(p *providers.Provider, wire string, now time.Time) bool {
	_, ready, fresh := ServedListed(p, wire, now)
	return fresh && !ready
}

// PinMisfit reports a pin that a Databricks row's fresh listing lacks as READY: another provider's id, not this row's.
func PinMisfit(p *providers.Provider, wire string, now time.Time) bool {
	return p != nil && p.Preset == providers.PresetDatabricks && wire != "" && ListedNotReady(p, wire, now)
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
	return update(func(s State) {
		account := ProviderAccount(p.Name)
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

// NextServed marks the failed plain model id gone on p and returns the next READY Responses model, "" when none.
func NextServed(p *providers.Provider, failed string) string {
	if err := MarkUnserved(p, failed); err != nil {
		fmt.Fprintf(os.Stderr, "served: could not mark %s unavailable: %v\n", failed, err)
	}
	next, _ := ServedFirst(p, true, time.Now())
	if next == "" || next == failed {
		return ""
	}
	fmt.Fprintf(os.Stderr, "served: %s unavailable on %s; using %s\n", failed, p.Name, next)
	return next
}
