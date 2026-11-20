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
	pm := pinnedModel(agent, alias, cell, p, model, now)
	if model != "" && unserved(p, pm, now) {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, pm + " is not served on " + p.Name})
		return nil
	}
	c := Cell{Model: pm, Effort: cmp.Or(effort, cell.Effort), Account: accountFrom(ctx)(pin), Price: cell.Price}
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
func pinnedModel(agent, alias string, cell Cell, p *providers.Provider, modelPin string, now time.Time) string {
	m := selectPinnedModel(agent, alias, cell, p, modelPin)
	if modelPin != "" && agentid.Routing(agent).GoogleOrDatabricks && PinMisfit(p, m, now) {
		// a pin this row does not serve: the route's own default
		modelPin, m = "", selectPinnedModel(agent, alias, cell, p, "")
	}
	if modelPin == "" && unserved(p, m, now) {
		if alt, _ := ServedFirst(p, agentid.Routing(agent).ResponsesRoute, now); alt != "" {
			return alt
		}
	}
	return m
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

// RotationPool is the served pool agent rotates through on p at alias ("" = every tier); the agent package sets it.
var RotationPool func(agent, alias string, p *providers.Provider, tools bool, now time.Time) []string

// RotationRow reports a row a rotation pin runs on: a Databricks workspace or a GitHub Copilot login.
func RotationRow(p *providers.Provider) bool {
	return p != nil && (p.Preset == providers.PresetDatabricks || p.Provider == "github")
}

// poolFor is empty until the agent package supplies the served pool.
func poolFor(agent, alias string, p *providers.Provider, now time.Time) []string {
	if RotationPool != nil {
		return RotationPool(agent, alias, p, false, now)
	}
	return nil
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

// PickRotation orders by hist under one lock, keeping holds on account; nil p skips the listing.
// A model in dear (entry -> turns) is passed over, sent to the back, until it has sat out turns-1 of its turns.
func PickRotation(ctx context.Context, agent, account, hist string, p *providers.Provider, pool []string, dear map[string]int, now time.Time) (string, error) {
	if p != nil {
		pool = ServedPool(p, pool, now)
	}
	var model string
	err := updateCtx(ctx, func(s State) {
		e := s[hist]
		selected, passed := leastRecent(s[account], e, agent, p, pool, dear, now)
		if selected == "" {
			return
		}
		model = providers.SpellPin(p, selected)
		stamp := now // Lock acquisition can differ from caller timestamp order.
		for _, entry := range s {
			for _, at := range entry.Discovery {
				if !stamp.After(at) {
					stamp = at.Add(time.Nanosecond)
				}
			}
		}
		e.Deferred = maps.Clone(e.Deferred)
		for _, m := range passed {
			k := DiscoverySkipKey(agent, m)
			e.Discovery = withTime(e.Discovery, k, stamp)
			if e.Deferred == nil {
				e.Deferred = map[string]int{}
			}
			e.Deferred[k]++
		}
		delete(e.Deferred, DiscoverySkipKey(agent, selected))
		e.Discovery = withTime(e.Discovery, DiscoverySkipKey(agent, selected), stamp)
		s[hist] = e
	})
	return model, err
}

// PeekRotation is the entry PickRotation would select now, recording nothing; "" when none qualifies.
func PeekRotation(agent, account, hist string, p *providers.Provider, pool []string, dear map[string]int, now time.Time) string {
	if p != nil {
		pool = ServedPool(p, pool, now)
	}
	s := sharedState()
	if sel, _ := leastRecent(s[account], s[hist], agent, p, pool, dear, now); sel != "" {
		return providers.SpellPin(p, sel)
	}
	return ""
}

// leastRecent is agent's spellable pool entry picked longest ago, past skips and live model holds; "" when none.
// passed lists dear entries sat out for it; a dear entry with nothing else to pick is never passed over.
func leastRecent(e, history Entry, agent string, p *providers.Provider, pool []string, dear map[string]int, now time.Time) (string, []string) {
	var passed []string
	for {
		selected := oldest(e, history, agent, p, pool, passed, now)
		if selected == "" || history.Deferred[DiscoverySkipKey(agent, selected)]+1 >= dear[selected] {
			return selected, passed
		}
		if oldest(e, history, agent, p, pool, append(slices.Clone(passed), selected), now) == "" {
			return selected, passed
		}
		passed = append(passed, selected)
	}
}

// oldest is the pool entry picked longest ago outside skip, past skips and live model holds.
func oldest(e, history Entry, agent string, p *providers.Provider, pool, skip []string, now time.Time) string {
	var selected string
	for _, entry := range pool {
		if slices.Contains(skip, entry) || providers.SpellPin(p, entry) == "" || HeldUntil(e, agent, entry, now) != nil {
			continue
		}
		if selected == "" || history.Discovery[DiscoverySkipKey(agent, entry)].Before(history.Discovery[DiscoverySkipKey(agent, selected)]) {
			selected = entry
		}
	}
	return selected
}

// ServedPool drops entries a fresh listing marks not READY; unchanged when the switch is off or the cache cold.
func ServedPool(p *providers.Provider, pool []string, now time.Time) []string {
	ready, fresh := servedView(p, now)
	if !fresh {
		return pool
	}
	return slices.DeleteFunc(slices.Clone(pool), func(entry string) bool {
		return !ready(entry) && providers.SpellPin(p, entry) != ""
	})
}

// DiscoverySkipTTL is how long a model that failed for an agent stays out of that agent's rotation.
const DiscoverySkipTTL = 7 * 24 * time.Hour

// withTime returns a copy of m with k set to t, so a shared cached map is never mutated.
func withTime(m map[string]time.Time, k string, t time.Time) map[string]time.Time {
	m = maps.Clone(m)
	if m == nil {
		m = make(map[string]time.Time)
	}
	m[k] = t
	return m
}

// DiscoverySkipKey is the Discovery and DiscoverySkip map key for agent and entry.
func DiscoverySkipKey(agent, entry string) string { return agent + "|" + entry }

// HeldUntil is when a live quota hold or capability skip on e keeps agent's entry out of a pick; nil when none.
func HeldUntil(e Entry, agent, entry string, now time.Time) *time.Time {
	if until := e.ModelDead[ModelKey(entry)]; until.After(now) {
		return &until
	}
	if at, ok := e.DiscoverySkip[DiscoverySkipKey(agent, entry)]; ok && now.Sub(at) < DiscoverySkipTTL {
		until := at.Add(DiscoverySkipTTL)
		return &until
	}
	return nil
}

// stateLockWait matches paths.WithFileLock's wait; updateCtx polls the lock in stateLockSlice tries until then.
const stateLockWait, stateLockSlice = 2 * time.Minute, 100 * time.Millisecond

// updateCtx is update that also gives up once ctx ends, so a cancelled ask never waits out a held lock.
func updateCtx(ctx context.Context, fn func(State)) error {
	for deadline := time.Now().Add(stateLockWait); ; {
		ran, err := tryUpdate(stateLockSlice, fn)
		switch {
		case ran || err != nil:
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case time.Now().After(deadline):
			return fmt.Errorf("%w: %s.lock", paths.ErrLockTimeout, StatePath())
		}
	}
}

// SkipDiscovery marks a pool entry unusable; returns "" for no match or a persistence error, reported on stderr.
func SkipDiscovery(ctx context.Context, agent string, p *providers.Provider, wire string) string {
	var hit string
	err := updateCtx(ctx, func(s State) {
		for _, entry := range poolFor(agent, "", p, time.Now()) {
			if providers.SpellPin(p, entry) != wire {
				continue
			}
			account := ProviderAccount(p.Name)
			e := s[account]
			e.DiscoverySkip = withTime(e.DiscoverySkip, DiscoverySkipKey(agent, entry), time.Now())
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
	warnedPool.Do(func() { fmt.Fprintf(os.Stderr, "warning: %v; ignoring the discovery allowlist\n", err) })
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
