package router

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// gatePinned gates a non-claude agent pinned to pin: exactly one candidate on that profile, or a Skip.
func (t *Table) gatePinned(
	ctx context.Context, reg *providers.Registry, tier, runner, pin string, spec providers.AgentPinSpec, st State, o Opts, now time.Time,
	configured func(runner string, c Cell) error, res *Result,
) ([]Candidate, bool) {
	if runner == "opencode" && slices.Contains(providers.RouteOpencodeSubIDs, pin) {
		return t.gateOpencodeCandidate(ctx, tier, pin, st, o, now, configured, res)
	}
	return t.gateProfile(ctx, reg, tier, runner, pin, spec.Model, "", st, o, now, configured, res)
}

// gateProfile applies the configured and auth checks for an agent on a profile.
func (t *Table) gateProfile(
	ctx context.Context, reg *providers.Registry, tier, runner, pin, model, effort string, st State, o Opts, now time.Time,
	configured func(runner string, c Cell) error, res *Result,
) ([]Candidate, bool) {
	cell, ok := t.Cell(runner, tier)
	if !ok {
		res.Skipped = append(res.Skipped, Skip{runner, GateRow, "no " + tier + " row"})
		return nil, false
	}
	var p *providers.Provider
	if reg != nil {
		p = reg.Providers[pin]
	}
	if p == nil {
		res.Skipped = append(res.Skipped, Skip{runner, GateRow, "pinned profile " + pin + " is not in the registry"})
		return nil, false
	}
	if !providers.CanServe(runner, p) || !providers.CredentialPresent(p) {
		res.Skipped = append(res.Skipped, Skip{runner, GateAvailable, "pinned profile " + pin + " cannot serve " + runner + " (no route or credential)"})
		return nil, false
	}
	c := Cell{Model: pinnedModel(runner, tier, cell, p, model, now), Effort: cmp.Or(effort, cell.Effort), Account: ProviderAccount(pin), Price: cell.Price}
	if sp := providers.SpellPin(p, model); model != "" && model != BYOKModel && sp != "" && c.Model != sp {
		res.Skipped = append(res.Skipped, Skip{runner, GateAvailable, "pinned model " + sp + " is not READY on " + pin + "; using " + c.Model})
	}
	if err := configured(runner, c); err != nil {
		res.Skipped = append(res.Skipped, Skip{runner, GateAvailable, "not configured: " + firstLine(err.Error())})
		return nil, false
	}
	billing := t.Billing[c.Account]
	if billing == "" {
		billing = BillSubscription
		if p.APIKeyAuth() {
			billing = BillCredits
		}
	}
	cand, ok := t.gateAccount(ctx, runner, c, billing, st, o, now, res)
	if !ok {
		return nil, false
	}
	cand.Profile = pin
	return []Candidate{cand}, true
}

// pinnedModel resolves the pin's model, then swaps one a fresh served listing marks not READY for the first READY one.
func pinnedModel(runner, tier string, cell Cell, p *providers.Provider, modelPin string, now time.Time) string {
	if modelPin == BYOKModel {
		model, _ := PinnedModel(runner, tier, cell, p, modelPin)
		return model
	}
	m := selectPinnedModel(runner, tier, cell, p, modelPin)
	if p != nil && p.Preset == providers.PresetDatabricks {
		if ready, fresh := ServedReady(p, m, now); fresh && !ready {
			if alt, _ := ServedFirst(p, runner == "codex", now); alt != "" {
				return alt
			}
		}
	}
	return m
}

// selectPinnedModel: model pin if p serves it, else own-account cell, else (codex) a Responses model, else the slot.
func selectPinnedModel(runner, tier string, cell Cell, p *providers.Provider, modelPin string) string {
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
	m := p.WireModel(cmp.Or(p.Models[tier], cell.Model))
	if runner == "codex" && !providers.ServesResponses(p, m) {
		return cmp.Or(providers.FirstResponsesModel(p), m)
	}
	return m
}
