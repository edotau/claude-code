package router

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

// DatabricksResponses: the session/pinned profile is a signed-in Databricks row with a Responses route + catalog model.
func DatabricksResponses(reg *providers.Registry) (profile, model string, ok bool) {
	if reg == nil {
		return "", "", false
	}
	profile = cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile))
	p := reg.Providers[profile]
	if p == nil || p.Preset != providers.PresetDatabricks || !vendors.CanServe("codex", p) || !providers.CredentialPresent(p) {
		return "", "", false
	}
	if model = providers.FirstResponsesModel(p); model != "" {
		return profile, model, true
	}
	return "", "", false
}

// pickGate is the per-Pick state shared by the gates: the registry, loaded once, and copilot accounts already held.
type pickGate struct {
	reg  *providers.Registry
	held map[string]bool
}

// codexHop is the model and effort codex's Databricks hops run for tier: codex's own pick respelled for the
// profile when it serves Responses, else the profile's first Responses model.
func (t *Table) codexHop(reg *providers.Registry, tier string) (profile, model, effort string, ok bool) {
	profile, first, ok := DatabricksResponses(reg)
	if !ok {
		return "", "", "", false
	}
	cell, _ := t.Cell("codex", tier)
	if pin, _ := providers.CodexModel(); pin != "" {
		cell.Model = pin
	}
	if wire := reg.Providers[profile].WireModel(cell.Model); cell.Model != "" && providers.ServesResponses(reg.Providers[profile], wire) {
		return profile, wire, cell.Effort, true
	}
	return profile, first, cell.Effort, true
}

// gateDatabricksCodex is codex's Databricks candidate at tier: the same configured/auth/exhausted gates as any
// runner, on account databricks·<profile>. ok is false (no Skip) when Databricks is off or serves no usable model.
func (t *Table) gateDatabricksCodex(
	pg *pickGate, tier string, st State, now time.Time, configured func(runner string, c Cell) error, res *Result,
) (Candidate, bool) {
	reg := pg.reg
	profile, model, effort, ok := t.codexHop(reg, tier)
	if !ok {
		return Candidate{}, false
	}
	c := Cell{Model: model, Effort: effort, Account: ProviderAccount(profile)}
	name := "codex·" + c.Account
	if err := configured("codex", c); err != nil {
		res.Skipped = append(res.Skipped, Skip{name, GateAvailable, "not configured: " + firstLine(err.Error())})
		return Candidate{}, false
	}
	if reason, marked := st.accountAuthMarked(c.Account, now); marked {
		res.Skipped = append(res.Skipped, Skip{name, GateAuth, authSkipReason(reason, c.Account)})
		return Candidate{}, false
	}
	e := st[c.Account]
	if e.Status(now) == StateExhausted {
		res.Skipped = append(res.Skipped, Skip{name, GateExhausted, ExhaustedReason(c.Account, e, now)})
		return Candidate{}, false
	}
	billing := BillSubscription
	if reg.Providers[profile].APIKeyAuth() {
		billing = BillCredits
	}
	return Candidate{Agent: "codex", Cell: c, State: e.Status(now), Billing: billing, Profile: profile}, true
}

// gateCandidate gates one runner (off, pinned or unpinned); codex and copilot also offer candidates on the session profile.
func (t *Table) gateCandidate(
	ctx context.Context, pg *pickGate, tier, runner string, pins map[string]providers.AgentPinSpec, st State, o Opts, now time.Time,
	configured func(runner string, c Cell) error, res *Result,
) ([]Candidate, bool) {
	spec := pins[runner]
	if spec.Off {
		res.Skipped = append(res.Skipped, Skip{runner, GateOff, "deactivated (" + strings.ToUpper(runner) + "_ACTIVE=off)"})
		return nil, false
	}
	var group []Candidate
	if runner != "claude" && spec.Profile != "" {
		group, _ = t.gatePinned(ctx, pg.reg, tier, runner, spec.Profile, spec, st, o, now, configured, res)
	} else {
		group, _ = t.gateRunner(ctx, tier, runner, st, o, now, configured, res)
		if runner == "gemini" && spec.Model != "" { // native agy login: the GEMINI_MODEL pin beats the table cell
			for i := range group {
				group[i].Model = spec.Model
			}
		}
	}
	if runner == "copilot" {
		group = append(group, t.gateSessionCopilot(ctx, pg, tier, "", "", spec, group, st, o, now, configured, res)...)
	}
	if runner == "codex" && spec.Profile == "" {
		if d, ok := t.gateDatabricksCodex(pg, tier, st, now, configured, res); ok && !hasProfileAccount(group, d.Account) {
			group = append(group, d)
		}
		if res.Want == "codex" {
			if _, model, effort, ok := t.codexHop(pg.reg, tier); ok {
				hop := t.gateSessionCopilot(ctx, pg, tier, model, effort, pins["copilot"], nil, st, o, now, configured, res)
				for _, c := range hop {
					pg.held[c.Account] = true
				}
				group = append(group, hop...)
			}
		}
	}
	return group, len(group) > 0
}

// gateSessionCopilot checks copilot on the session's Databricks profile.
func (t *Table) gateSessionCopilot(
	ctx context.Context, pg *pickGate, tier, model, effort string, spec providers.AgentPinSpec, own []Candidate, st State, o Opts, now time.Time,
	configured func(runner string, c Cell) error, res *Result,
) []Candidate {
	if spec.Off {
		return nil
	}
	reg := pg.reg
	if reg == nil {
		return nil
	}
	if spec.Profile != "" {
		p := reg.Providers[spec.Profile]
		if p == nil || p.Provider != "github" {
			return nil
		}
	}
	profile := cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile))
	p := reg.Providers[profile]
	if p == nil || p.Preset != providers.PresetDatabricks || !vendors.CanServe("copilot", p) || !providers.CredentialPresent(p) {
		return nil
	}
	account := ProviderAccount(profile)
	if hasProfileAccount(own, account) || (own == nil && model == "" && pg.held[account]) {
		return nil
	}
	if own == nil {
		if cell, ok := t.Cell("copilot", tier); ok {
			if spec.Profile != "" {
				cell.Account = ProviderAccount(spec.Profile)
			} else if o.Account != nil {
				if a, _ := o.Account("copilot", cell); a != "" {
					cell.Account = a
				}
			}
			if cell.Account == account {
				return nil
			}
		}
	}
	group, _ := t.gateProfile(ctx, reg, tier, "copilot", profile, model, effort, st, o, now, configured, res)
	return group
}

func hasProfileAccount(group []Candidate, account string) bool {
	for _, c := range group {
		if c.Account == account {
			return true
		}
	}
	return false
}
