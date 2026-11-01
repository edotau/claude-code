package router

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
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
	if sp := providers.SpellPin(p, model); model != "" && model != BYOKModel && sp != "" && c.Model != sp {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "pinned model " + sp + " is not READY on " + pin + "; using " + c.Model})
	}
	if err := configured(agent, c); err != nil {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "not configured: " + textutil.FirstLine(strings.TrimSpace(err.Error()))})
		return nil, false
	}
	billing := t.Billing[c.Account]
	if billing == "" {
		billing = BillSubscription
		if p.APIKeyAuth() {
			billing = BillCredits
		}
	}
	cand, ok := t.gateAccount(ctx, agent, c, billing, st, o, now, res)
	if !ok {
		return nil, false
	}
	cand.Profile = pin
	return []Candidate{cand}, true
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
