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
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
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
	if agentid.Routing(agent).SubLadder && slices.Contains(providers.RouteOpencodeSubIDs, pin) {
		return t.gateOpencodeCandidate(ctx, alias, pin, st, o, now, configured, res)
	}
	if agentid.Routing(agent).GoogleOrDatabricks && reg != nil && !geminiRow(reg, pin) {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "gemini runs on Google or a Databricks row, not " + pin})
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
	pm, _ := pinnedModel(agent, alias, cell, p, o.modelPin(model), false, now)
	if pin := o.modelPin(model); pin != "" && pin != DiscoveryModel && unserved(p, pm, now) {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, pm + " is not served on " + p.Name})
		return nil
	}
	c := Cell{Model: pm, Effort: cmp.Or(effort, cell.Effort), Account: ProviderAccount(pin), Price: cell.Price}
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

// DefaultBilling is credits for an API-key provider, else subscription.
func DefaultBilling(p *providers.Provider) string {
	if p.APIKeyAuth() {
		return BillCredits
	}
	return BillSubscription
}

// unserved reports a Databricks model the fresh listing marks not READY.
func unserved(p *providers.Provider, m string, now time.Time) bool {
	if p == nil || p.Preset != providers.PresetDatabricks {
		return false
	}
	ready, fresh := ServedReady(p, m, now)
	return fresh && !ready
}

// pinnedModel resolves pins, swaps an unready alias pick for a READY model (never an explicit pin).
func pinnedModel(agent, alias string, cell Cell, p *providers.Provider, modelPin string, stamp bool, now time.Time) (string, string) {
	if modelPin == DiscoveryModel {
		return discoveryPinnedModel(agent, alias, cell, p, stamp, now)
	}
	m := selectPinnedModel(agent, alias, cell, p, modelPin)
	if modelPin == "" && unserved(p, m, now) {
		if alt, _ := ServedFirst(p, agentid.Routing(agent).ResponsesRoute, now); alt != "" {
			return alt, ""
		}
	}
	return m, ""
}

func discoveryPinnedModel(agent, alias string, cell Cell, p *providers.Provider, stamp bool, now time.Time) (string, string) {
	why := ""
	switch {
	case !agentid.Routing(agent).Discovery:
		why = "discovery requires copilot or opencode; using alias"
	case !RotationRow(p):
		why = "discovery requires a Databricks or GitHub row; using alias"
	default:
		pool, model, err := poolFor(agent, alias, p, now), "", error(nil)
		if stamp {
			model, err = pickPool(agent, p, pool, now)
		} else {
			model = PeekRotation(agent, ProviderAccount(p.Name), p, pool, now)
		}
		switch {
		case err != nil:
			why = "discovery state unavailable (" + stateErrClass(err) + "); using alias"
		case model == "":
			why = "discovery pool has no served model; using alias"
		default:
			return model, ""
		}
	}
	model, _ := pinnedModel(agent, alias, cell, p, "", stamp, now)
	return model, why
}

// selectPinnedModel: model pin, else own-account cell, else Databricks tier, else alias; codex gets a Responses model.
func selectPinnedModel(agent, alias string, cell Cell, p *providers.Provider, modelPin string) string {
	if p == nil {
		return cell.Model
	}
	if modelPin != "" {
		if m := providers.SpellPin(p, providers.ConcreteModel(agent, p, modelPin)); m != "" {
			return m
		}
	}
	if ProviderAccount(p.Name) == cell.Account {
		return cell.Model
	}
	m := cmp.Or(DatabricksModel(agent, alias, p), p.WireModel(cmp.Or(p.Models[alias], cell.Model)))
	if agentid.Routing(agent).ResponsesRoute && !providers.ServesResponses(p, m) {
		return cmp.Or(providers.FirstResponsesModel(p), m)
	}
	return m
}

// DiscoveryModel requests the least-recently-picked model in the shared Databricks pool.
const DiscoveryModel = "rotate"

// RotationPool is the served pool agent rotates through on p at alias ("" = every tier); the agent package sets it.
var RotationPool func(agent, alias string, p *providers.Provider, now time.Time) []string

// RotationRow reports a row a rotation pin runs on: a Databricks workspace or a GitHub Copilot login.
func RotationRow(p *providers.Provider) bool {
	return p != nil && (p.Preset == providers.PresetDatabricks || p.Provider == "github")
}

// poolFor is RotationPool's pool for agent on p at alias, else the vault overlay or seed list.
func poolFor(agent, alias string, p *providers.Provider, now time.Time) []string {
	if RotationPool != nil {
		return RotationPool(agent, alias, p, now)
	}
	return DiscoveryPool()
}

// PoolOverlay is the vault's discovery-models list when set and valid: the rotation's allowlist.
func PoolOverlay() ([]string, bool) { return vaultDiscoveryPool() }

// IsPoolModel reports whether model is in agent's rotation pool as p serves it, at any tier.
func IsPoolModel(agent string, p *providers.Provider, model string) bool {
	if !RotationRow(p) {
		return false
	}
	wire := strings.TrimPrefix(model, p.Name+":")
	return slices.ContainsFunc(poolFor(agent, "", p, time.Now()), func(entry string) bool {
		return providers.SpellPin(p, entry) == wire
	})
}

// PinnedModel resolves a pin and reports why a Discovery request fell back to the alias; stamp records the pick.
func PinnedModel(agent, alias string, cell Cell, p *providers.Provider, pin string, stamp bool) (string, string) {
	return pinnedModel(agent, alias, cell, p, pin, stamp, time.Now())
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

// pickPool is PickRotation on p's own account.
func pickPool(agent string, p *providers.Provider, pool []string, now time.Time) (string, error) {
	return PickRotation(agent, ProviderAccount(p.Name), p, pool, now)
}

// PickRotation selects and records agent's least-recent pool entry on account under one lock; nil p skips the listing.
func PickRotation(agent, account string, p *providers.Provider, pool []string, now time.Time) (string, error) {
	if p != nil {
		pool = servedPool(p, pool, now)
	}
	var model string
	err := update(func(s State) {
		e := s[account]
		selected := leastRecent(e, agent, p, pool, now)
		if selected == "" {
			return
		}
		model = providers.SpellPin(p, selected)
		e.Discovery = withTime(e.Discovery, discoverySkipKey(agent, selected), now)
		s[account] = e
	})
	return model, err
}

// PeekRotation is the entry PickRotation would select now, recording nothing; "" when none qualifies.
func PeekRotation(agent, account string, p *providers.Provider, pool []string, now time.Time) string {
	if p != nil {
		pool = servedPool(p, pool, now)
	}
	if sel := leastRecent(LoadState()[account], agent, p, pool, now); sel != "" {
		return providers.SpellPin(p, sel)
	}
	return ""
}

// leastRecent is agent's spellable pool entry picked longest ago, past skips and live model holds; "" when none.
func leastRecent(e Entry, agent string, p *providers.Provider, pool []string, now time.Time) string {
	var selected string
	for _, entry := range pool {
		if providers.SpellPin(p, entry) == "" || discoverySkipped(e, agent, entry, now) || e.ModelDead[ModelKey(entry)].After(now) {
			continue
		}
		if selected == "" || e.Discovery[discoverySkipKey(agent, entry)].Before(e.Discovery[discoverySkipKey(agent, selected)]) {
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
		for _, entry := range poolFor(agent, "", p, time.Now()) {
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

// warnedPool limits the unusable-overlay warning to one per process.
var warnedPool sync.Once

func vaultDiscoveryPool() ([]string, bool) {
	pool, err := poolCache.Get(vault.Path(), readPoolOverlay)
	if err != nil {
		warnPool(err)
	}
	if err != nil || len(pool) == 0 {
		return nil, false
	}
	return slices.Clone(pool), true
}

func warnPool(err error) {
	warnedPool.Do(func() { fmt.Fprintf(os.Stderr, "warning: %v; using the built-in discovery pool\n", err) })
}

// readPoolOverlay decodes the vault's Discovery pool overlay; nil when unset or invalid, an error on a failed read.
func readPoolOverlay() ([]string, error) {
	var out []string
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.PoolOverlay()
		if !ok || raw == "" {
			return nil
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			out = nil
			warnPool(fmt.Errorf("vault overlay %s is not a JSON list: %w", vault.PoolKey, err))
		}
		return nil
	})
	return catalogPool(out), err
}

// warnedPoolDrop limits the dropped-entries warning to one per process.
var warnedPoolDrop sync.Once

// catalogPool drops entries the model catalog does not know, so a retired or mistyped id never gets picked.
func catalogPool(pool []string) []string {
	var kept, dropped []string
	for _, entry := range pool {
		if _, _, ok := models.Resolve(entry); ok {
			kept = append(kept, entry)
		} else {
			dropped = append(dropped, entry)
		}
	}
	if len(dropped) > 0 {
		warnedPoolDrop.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: vault overlay %s: skipping ids not in the model catalog: %s\n",
				vault.PoolKey, strings.Join(dropped, ", "))
		})
	}
	return kept
}

// DiscoveryRepick skips the failed model for agent, then returns the next pool pick ("" if unchanged).
func DiscoveryRepick(agent, alias string, cell Cell, p *providers.Provider, prefix string) func(string) string {
	return func(failed string) string {
		wire := strings.TrimPrefix(failed, prefix)
		if SkipDiscovery(agent, p, wire) == "" {
			return ""
		}
		next, _ := PinnedModel(agent, alias, cell, p, DiscoveryModel, true)
		if next == "" || next == wire {
			return ""
		}
		fmt.Fprintf(os.Stderr, "discovery: %s skipped for %s (model capability)\n", wire, agent)
		return prefix + next
	}
}
