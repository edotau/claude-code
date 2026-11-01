package router

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// BYOKModel requests the least-recently-picked model in the shared Databricks pool.
const BYOKModel = "byok"

// IsPoolModel reports whether model is a member of the BYOK pool as p serves it.
func IsPoolModel(p *providers.Provider, model string) bool {
	if p == nil || p.Preset != providers.PresetDatabricks {
		return false
	}
	wire := strings.TrimPrefix(model, p.Name+":")
	return slices.ContainsFunc(BYOKPool(), func(entry string) bool {
		return providers.SpellPin(p, entry) == wire
	})
}

// PinnedModel resolves a pin and reports why a BYOK request fell back to the alias.
func PinnedModel(agent, alias string, cell Cell, p *providers.Provider, pin string) (string, string) {
	now := time.Now()
	if pin != BYOKModel {
		return pinnedModel(agent, alias, cell, p, pin, now), ""
	}
	if agent != "copilot" && agent != "opencode" {
		return pinnedModel(agent, alias, cell, p, "", now), "byok requires copilot or opencode; using alias"
	}
	if p == nil || p.Preset != providers.PresetDatabricks {
		return pinnedModel(agent, alias, cell, p, "", now), "byok requires Databricks; using alias"
	}
	model, err := pickPool(agent, p, BYOKPool(), now)
	if err != nil {
		return pinnedModel(agent, alias, cell, p, "", now), "byok state unavailable; using alias"
	}
	if model == "" {
		return pinnedModel(agent, alias, cell, p, "", now), "byok pool has no served model; using alias"
	}
	return model, ""
}

// pickPool selects and records under the same lock, so simultaneous picks see each other's timestamps.
func pickPool(agent string, p *providers.Provider, pool []string, now time.Time) (string, error) {
	pool = servedPool(p, pool, now)
	var selected, model string
	err := Update(func(s State) {
		account := ProviderAccount(p.Name)
		e := s[account]
		for _, entry := range pool {
			wire := providers.SpellPin(p, entry)
			if wire != "" && !byokSkipped(e, agent, entry, now) && (selected == "" || e.BYOK[entry].Before(e.BYOK[selected])) {
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
		return !ready(entry) && providers.SpellPin(p, entry) != ""
	})
}

// byokSkipTTL is how long a model that failed for an agent stays out of that agent's rotation.
const byokSkipTTL = 7 * 24 * time.Hour

func byokSkipKey(agent, entry string) string { return agent + "|" + entry }

func byokSkipped(e Entry, agent, entry string, now time.Time) bool {
	at, ok := e.BYOKSkip[byokSkipKey(agent, entry)]
	return ok && now.Sub(at) < byokSkipTTL
}

// SkipBYOK marks the pool entry p serves as wire unusable for agent and returns it, "" when none matches.
func SkipBYOK(agent string, p *providers.Provider, wire string) string {
	var hit string
	_ = Update(func(s State) {
		for _, entry := range BYOKPool() {
			if providers.SpellPin(p, entry) != wire {
				continue
			}
			account := ProviderAccount(p.Name)
			e := s[account]
			e.BYOKSkip = maps.Clone(e.BYOKSkip)
			if e.BYOKSkip == nil {
				e.BYOKSkip = make(map[string]time.Time)
			}
			e.BYOKSkip[byokSkipKey(agent, entry)] = time.Now()
			s[account] = e
			hit = entry
			return
		}
	})
	return hit
}

// byokPoolKey is the vault Overlay entry holding the BYOK pool: a JSON array of Databricks wire ids.
const byokPoolKey = "byok-models"

// DatabricksBYOKPool is the seeded BYOK pool of Databricks wire ids, used when the vault overlay is unset or invalid.
var DatabricksBYOKPool = []string{
	"databricks-grok-4-7",
	"databricks-glm-5-3",
	"databricks-gpt-oss-120b",
	"databricks-gpt-6-astra",
	"databricks-gemini-3-8-flash",
	"databricks-gpt-6-1-sol",
}

// BYOKPool is the BYOK model pool: the vault overlay if set and valid, else the DatabricksBYOKPool seed.
func BYOKPool() []string {
	if p, ok := vaultBYOKPool(); ok {
		return p
	}
	return slices.Clone(DatabricksBYOKPool)
}

func vaultBYOKPool() ([]string, bool) {
	var out []string
	var found bool
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, byokPoolKey)
		if !ok || raw == "" {
			return nil
		}
		found = json.Unmarshal([]byte(raw), &out) == nil && len(out) > 0
		return nil
	})
	if err != nil || !found {
		return nil, false
	}
	return out, true
}
