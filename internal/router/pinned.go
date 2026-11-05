package router

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// gatePinned gates a non-claude agent pinned to pin: exactly one candidate on that profile, or a Skip.
func (t *RouteTable) gatePinned(
	ctx context.Context, reg *providers.Registry, alias, agent, pin string, spec PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) ([]Candidate, bool) {
	if agent == "opencode" && slices.Contains(providers.RouteOpencodeSubIDs, pin) {
		return t.gateOpencodeCandidate(ctx, alias, pin, st, o, now, configured, res)
	}
	return t.gateProfile(ctx, reg, alias, agent, pin, spec.Model, "", st, o, now, configured, res)
}

// gateProfile applies the configured and auth checks for an agent on a profile.
func (t *RouteTable) gateProfile(
	ctx context.Context, reg *providers.Registry, alias, agent, pin, model, effort string, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) ([]Candidate, bool) {
	cell, ok := t.Cell(agent, alias)
	if !ok {
		res.Skipped = append(res.Skipped, Skip{agent, GateRow, "no " + alias + " row"})
		return nil, false
	}
	var p *providers.Provider
	if reg != nil {
		p = reg.Providers[pin]
	}
	if p == nil {
		res.Skipped = append(res.Skipped, Skip{agent, GateRow, "pinned profile " + pin + " is not in the registry"})
		return nil, false
	}
	if !o.canServe(agent, p) || !providers.CredentialPresent(p) {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "pinned profile " + pin + " cannot serve " + agent + " (no route or credential)"})
		return nil, false
	}
	c := Cell{Model: pinnedModel(agent, alias, cell, p, model, now), Effort: cmp.Or(effort, cell.Effort), Account: ProviderAccount(pin), Price: cell.Price}
	noteNotReady(res, agent, pin, model, c.Model, p)
	if err := configured(agent, c); err != nil {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "not configured: " + textutil.FirstLine(strings.TrimSpace(err.Error()))})
		return nil, false
	}
	billing := cmp.Or(t.Billing[c.Account], defaultBilling(p))
	cand, ok := t.gateAccount(ctx, agent, c, billing, st, o, now, res)
	if !ok {
		return nil, false
	}
	cand.Profile = pin
	return []Candidate{cand}, true
}

// noteNotReady records a Skip when the pinned model is spelled but swapped for another because it is not READY.
func noteNotReady(res *PickResult, agent, pin, model, used string, p *providers.Provider) {
	sp := providers.SpellPin(p, model)
	if model != "" && model != BYOKModel && sp != "" && used != sp {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "pinned model " + sp + " is not READY on " + pin + "; using " + used})
	}
}

// defaultBilling is credits for an API-key provider, else subscription.
func defaultBilling(p *providers.Provider) string {
	if p.APIKeyAuth() {
		return BillCredits
	}
	return BillSubscription
}

// pinnedModel resolves the pin's model, then swaps one a fresh served listing marks not READY for the first READY one.
func pinnedModel(agent, alias string, cell Cell, p *providers.Provider, modelPin string, now time.Time) string {
	if modelPin == BYOKModel {
		model, _ := PinnedModel(agent, alias, cell, p, modelPin)
		return model
	}
	m := selectPinnedModel(agent, alias, cell, p, modelPin)
	if p != nil && p.Preset == providers.PresetDatabricks {
		if ready, fresh := ServedReady(p, m, now); fresh && !ready {
			if alt, _ := ServedFirst(p, agent == "codex", now); alt != "" {
				return alt
			}
		}
	}
	return m
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
		return pinnedModel(agent, alias, cell, p, "", now), "byok state unavailable (" + stateErrClass(err) + "); using alias"
	}
	if model == "" {
		return pinnedModel(agent, alias, cell, p, "", now), "byok pool has no served model; using alias"
	}
	return model, ""
}

// stateErrClass names why Update failed, for a note; the raw error carries paths, so it stays out.
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
