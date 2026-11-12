package router

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
)

// gatePinned gates a non-claude agent pinned to pin: exactly one candidate on that profile, or a Skip.
func (t *RouteTable) gatePinned(
	ctx context.Context, reg *providers.Registry, alias, agent, pin string, spec PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	if agent == "opencode" && slices.Contains(providers.RouteOpencodeSubIDs, pin) {
		return t.gateOpencodeCandidate(ctx, alias, pin, st, o, now, configured, res)
	}
	if agent == "gemini" && reg != nil && !geminiRow(reg, pin) {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "gemini runs on Google or a Databricks gateway row, not " + pin})
		return nil
	}
	return t.gateProfile(ctx, reg, alias, agent, pin, spec.Model, "", st, o, now, configured, res)
}

// gateProfile applies the configured and auth checks for an agent on a profile.
func (t *RouteTable) gateProfile(
	ctx context.Context, reg *providers.Registry, alias, agent, pin, model, effort string, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	cell, ok := t.Cell(agent, alias)
	if !ok {
		skipNoRow(res, agent, alias)
		return nil
	}
	var p *providers.Provider
	if reg != nil {
		p = reg.Providers[pin]
	}
	if p == nil {
		res.Skipped = append(res.Skipped, Skip{agent, gateRow, "pinned profile " + pin + " is not in the registry"})
		return nil
	}
	if !o.canServe(agent, p) || !providers.CredentialPresent(p) {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "pinned profile " + pin + " cannot serve " + agent + " (no route or credential)"})
		return nil
	}
	pm, _ := pinnedModel(agent, alias, cell, p, o.modelPin(model), now)
	c := Cell{Model: pm, Effort: cmp.Or(effort, cell.Effort), Account: ProviderAccount(pin), Price: cell.Price}
	noteNotReady(res, agent, pin, model, c.Model, p)
	if err := configured(agent, c); err != nil {
		skipNotConfigured(res, agent, err)
		return nil
	}
	billing := cmp.Or(t.Billing[c.Account], DefaultBilling(p))
	cand, ok := t.gateAccount(ctx, agent, c, billing, st, o, now, res)
	if !ok {
		return nil
	}
	cand.Profile = pin
	return []Candidate{cand}
}

// noteNotReady records a Skip when the pinned model is spelled but swapped for another because it is not READY.
func noteNotReady(res *PickResult, agent, pin, model, used string, p *providers.Provider) {
	sp := providers.SpellPin(p, model)
	if model != "" && model != DiscoveryModel && sp != "" && used != sp {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "pinned model " + sp + " is not READY on " + pin + "; using " + used})
	}
}

// DefaultBilling is credits for an API-key provider, else subscription.
func DefaultBilling(p *providers.Provider) string {
	if p.APIKeyAuth() {
		return BillCredits
	}
	return BillSubscription
}

// pinnedModel resolves the pin's model at now, then swaps one a fresh served listing marks not READY for the first
// READY one; the second result says why a Discovery request fell back to the alias.
func pinnedModel(agent, alias string, cell Cell, p *providers.Provider, modelPin string, now time.Time) (string, string) {
	if modelPin == DiscoveryModel {
		why := ""
		switch {
		case agent != "copilot" && agent != "opencode":
			why = "discovery requires copilot or opencode; using alias"
		case p == nil || p.Preset != providers.PresetDatabricks:
			why = "discovery requires Databricks; using alias"
		default:
			model, err := pickPool(agent, p, DiscoveryPool(), now)
			switch {
			case err != nil:
				why = "discovery state unavailable (" + stateErrClass(err) + "); using alias"
			case model == "":
				why = "discovery pool has no served model; using alias"
			default:
				return model, ""
			}
		}
		model, _ := pinnedModel(agent, alias, cell, p, "", now)
		return model, why
	}
	m := selectPinnedModel(agent, alias, cell, p, modelPin)
	if p != nil && p.Preset == providers.PresetDatabricks {
		if ready, fresh := ServedReady(p, m, now); fresh && !ready {
			if alt, _ := ServedFirst(p, agent == "codex", now); alt != "" {
				return alt, ""
			}
		}
	}
	return m, ""
}

// selectPinnedModel: model pin if p serves it, else own-account cell, else (codex) a Responses model, else the alias.
func selectPinnedModel(agent, alias string, cell Cell, p *providers.Provider, modelPin string) string {
	if p == nil {
		return cell.Model
	}
	if modelPin != "" {
		if m := providers.SpellPin(p, modelPin); m != "" {
			return m
		}
	}
	if ProviderAccount(p.Name) == cell.Account {
		return cell.Model
	}
	m := p.WireModel(cmp.Or(p.Models[alias], cell.Model))
	if agent == "codex" && !providers.ServesResponses(p, m) {
		return cmp.Or(providers.FirstResponsesModel(p), m)
	}
	return m
}

// DiscoveryModel requests the least-recently-picked model in the shared Databricks pool.
const DiscoveryModel = "discovery"

// IsPoolModel reports whether model is a member of the Discovery pool as p serves it.
func IsPoolModel(p *providers.Provider, model string) bool {
	if p == nil || p.Preset != providers.PresetDatabricks {
		return false
	}
	wire := strings.TrimPrefix(model, p.Name+":")
	return slices.ContainsFunc(DiscoveryPool(), func(entry string) bool {
		return providers.SpellPin(p, entry) == wire
	})
}

// PinnedModel resolves a pin and reports why a Discovery request fell back to the alias.
func PinnedModel(agent, alias string, cell Cell, p *providers.Provider, pin string) (string, string) {
	return pinnedModel(agent, alias, cell, p, pin, time.Now())
}

// stateErrClass names why update failed, for a note; the raw error carries paths, so it stays out.
func stateErrClass(err error) string {
	switch {
	case errors.Is(err, schemas.ErrNewer):
		return "written by a newer build"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, syscall.ENOSPC):
		return "disk full"
	}
	return "file error"
}

// pickPool selects and records under the same lock, so simultaneous picks see each other's timestamps.
func pickPool(agent string, p *providers.Provider, pool []string, now time.Time) (string, error) {
	pool = servedPool(p, pool, now)
	var model string
	err := update(func(s State) {
		account := ProviderAccount(p.Name)
		e := s[account]
		selected := leastRecent(e, agent, p, pool, now)
		model = providers.SpellPin(p, selected)
		if selected == "" {
			return
		}
		e.Discovery = withTime(e.Discovery, selected, now)
		s[account] = e
	})
	return model, err
}

// leastRecent is the spellable, unskipped pool entry picked longest ago; "" when none qualifies.
func leastRecent(e Entry, agent string, p *providers.Provider, pool []string, now time.Time) string {
	var selected string
	for _, entry := range pool {
		if providers.SpellPin(p, entry) != "" && !discoverySkipped(e, agent, entry, now) &&
			(selected == "" || e.Discovery[entry].Before(e.Discovery[selected])) {
			selected = entry
		}
	}
	return selected
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

// discoverySkipTTL is how long a model that failed for an agent stays out of that agent's rotation.
const discoverySkipTTL = 7 * 24 * time.Hour

// withTime returns a copy of m with k set to t, so a shared cached map is never mutated.
func withTime(m map[string]time.Time, k string, t time.Time) map[string]time.Time {
	m = maps.Clone(m)
	if m == nil {
		m = make(map[string]time.Time)
	}
	m[k] = t
	return m
}

func discoverySkipKey(agent, entry string) string { return agent + "|" + entry }

func discoverySkipped(e Entry, agent, entry string, now time.Time) bool {
	at, ok := e.DiscoverySkip[discoverySkipKey(agent, entry)]
	return ok && now.Sub(at) < discoverySkipTTL
}

// SkipDiscovery marks a pool entry unusable; returns "" for no match or a persistence error, reported on stderr.
func SkipDiscovery(agent string, p *providers.Provider, wire string) string {
	var hit string
	err := update(func(s State) {
		for _, entry := range DiscoveryPool() {
			if providers.SpellPin(p, entry) != wire {
				continue
			}
			account := ProviderAccount(p.Name)
			e := s[account]
			e.DiscoverySkip = withTime(e.DiscoverySkip, discoverySkipKey(agent, entry), time.Now())
			s[account] = e
			hit = entry
			return
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "discovery: could not persist model skip (%s)\n", stateErrClass(err))
		return ""
	}
	return hit
}

// databricksDiscoveryPool is the seeded Discovery pool of wire ids, used when the vault overlay is unset or invalid.
var databricksDiscoveryPool = []string{
	"databricks-grok-4-7",
	"databricks-glm-5-3",
	"databricks-gpt-oss-120b",
	"databricks-gpt-6-astra",
	"databricks-gemini-3-8-flash",
	"databricks-gpt-6-1-sol",
}

// DiscoveryPool is the Discovery model pool: the vault overlay if set and valid, else the databricksDiscoveryPool seed.
func DiscoveryPool() []string {
	if p, ok := vaultDiscoveryPool(); ok {
		return p
	}
	return slices.Clone(databricksDiscoveryPool)
}

// poolCache holds the vault's Discovery pool overlay per vault.db stamp.
var poolCache paths.StampCache[[]string]

func vaultDiscoveryPool() ([]string, bool) {
	pool, err := poolCache.Get(vault.Path(), readPoolOverlay)
	if err != nil || len(pool) == 0 {
		return nil, false
	}
	return slices.Clone(pool), true
}

// readPoolOverlay decodes the vault's Discovery pool overlay; nil when unset or invalid, an error on a failed read.
func readPoolOverlay() ([]string, error) {
	var out []string
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.PoolOverlay()
		if !ok || raw == "" {
			return nil
		}
		if json.Unmarshal([]byte(raw), &out) != nil {
			out = nil
		}
		return nil
	})
	return out, err
}
