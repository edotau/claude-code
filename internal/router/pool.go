package router

import (
	"maps"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/agent/vendors"
)

// BYOKModel requests the least-recently-picked model in the shared Databricks pool.
const BYOKModel = "byok"

// PinnedModel resolves a pin and reports why a BYOK request fell back to the tier slot.
func PinnedModel(runner, tier string, cell Cell, p *providers.Provider, pin string) (string, string) {
	now := time.Now()
	if pin != BYOKModel {
		return pinnedModel(runner, tier, cell, p, pin, now), ""
	}
	if runner != "copilot" && runner != "opencode" {
		return pinnedModel(runner, tier, cell, p, "", now), "byok requires copilot or opencode; using tier slot"
	}
	if p == nil || p.Preset != providers.PresetDatabricks {
		return pinnedModel(runner, tier, cell, p, "", now), "byok requires Databricks; using tier slot"
	}
	model, err := pickPool(runner, p, providers.BYOKPool(), now)
	if err != nil {
		return pinnedModel(runner, tier, cell, p, "", now), "byok state unavailable; using tier slot"
	}
	if model == "" {
		return pinnedModel(runner, tier, cell, p, "", now), "byok pool has no served model; using tier slot"
	}
	return model, ""
}

// pickPool selects and records under the same lock, so simultaneous picks see each other's timestamps.
func pickPool(runner string, p *providers.Provider, pool []string, now time.Time) (string, error) {
	pool = servedPool(p, pool, now)
	var selected, model string
	err := Update(func(s State) {
		account := ProviderAccount(p.Name)
		e := s[account]
		for _, entry := range pool {
			wire := vendors.SpellPin(p, entry)
			if wire != "" && !e.byokSkipped(runner, entry, now) && (selected == "" || e.BYOK[entry].Before(e.BYOK[selected])) {
				selected, model = entry, wire
			}
		}
		if selected == "" {
			return
		}
		e.BYOK = maps.Clone(e.BYOK)
		if e.BYOK == nil {
			e.BYOK = make(map[string]time.Time)
		}
		e.BYOK[selected] = now
		s[account] = e
	})
	return model, err
}

// servedPool drops entries a fresh listing marks not READY; unchanged when the switch is off or the cache cold.
func servedPool(p *providers.Provider, pool []string, now time.Time) []string {
	ready, fresh := servedView(p, now)
	if !fresh {
		return pool
	}
	return slices.DeleteFunc(slices.Clone(pool), func(entry string) bool {
		return !ready(entry) && vendors.SpellPin(p, entry) != ""
	})
}

// byokSkipTTL is how long a model that failed for an agent stays out of that agent's rotation.
const byokSkipTTL = 7 * 24 * time.Hour

func byokSkipKey(runner, entry string) string { return runner + "|" + entry }

func (e Entry) byokSkipped(runner, entry string, now time.Time) bool {
	at, ok := e.BYOKSkip[byokSkipKey(runner, entry)]
	return ok && now.Sub(at) < byokSkipTTL
}

// SkipBYOK marks the pool entry p serves as wire unusable for runner and returns it, "" when none matches.
func SkipBYOK(runner string, p *providers.Provider, wire string) string {
	var hit string
	_ = Update(func(s State) {
		for _, entry := range providers.BYOKPool() {
			if vendors.SpellPin(p, entry) != wire {
				continue
			}
			account := ProviderAccount(p.Name)
			e := s[account]
			e.BYOKSkip = maps.Clone(e.BYOKSkip)
			if e.BYOKSkip == nil {
				e.BYOKSkip = make(map[string]time.Time)
			}
			e.BYOKSkip[byokSkipKey(runner, entry)] = time.Now()
			s[account] = e
			hit = entry
			return
		}
	})
	return hit
}
