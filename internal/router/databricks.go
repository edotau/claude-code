package router

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// DatabricksResponses: the session/pinned profile is a signed-in Databricks row with a Responses route + catalog model.
func DatabricksResponses(reg *providers.Registry, now time.Time, canServe func(string, *providers.Provider) bool) (profile, model string, ok bool) {
	if reg == nil {
		return "", "", false
	}
	profile, p, ok := sessionDatabricks(reg, "codex", canServe)
	if !ok {
		return "", "", false
	}
	if first, fresh := ServedFirst(p, true, now); fresh { // a fresh listing proves what the workspace serves
		if first == "" {
			return "", "", false
		}
		return profile, first, true
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
func (t *RouteTable) codexHop(reg *providers.Registry, tier string, o PickOpts, now time.Time) (profile, model, effort string, ok bool) {
	profile, first, ok := DatabricksResponses(reg, now, o.canServe)
	if !ok {
		return "", "", "", false
	}
	cell, _ := t.Cell("codex", tier)
	p := reg.Providers[profile]
	cell.Model = o.codexModel(providers.Target{Provider: p, Model: cell.Model}).Model
	if wire := p.WireModel(cell.Model); cell.Model != "" && providers.ServesResponses(p, wire) {
		if ready, fresh := ServedReady(p, wire, now); fresh && !ready {
			return profile, first, cell.Effort, true
		}
		return profile, wire, cell.Effort, true
	}
	return profile, first, cell.Effort, true
}

// gateDatabricksCodex is codex's Databricks candidate at tier: the same configured/auth/exhausted gates as any
// runner, on account databricks·<profile>. ok is false (no Skip) when Databricks is off or serves no usable model.
func (t *RouteTable) gateDatabricksCodex(
	pg *pickGate, tier string, st State, o PickOpts, now time.Time, configured func(runner string, c Cell) error, res *PickResult,
) (Candidate, bool) {
	reg := pg.reg
	profile, model, effort, ok := t.codexHop(reg, tier, o, now)
	if !ok {
		return Candidate{}, false
	}
	c := Cell{Model: model, Effort: effort, Account: ProviderAccount(profile)}
	name := "codex·" + c.Account
	if err := configured("codex", c); err != nil {
		res.Skipped = append(res.Skipped, Skip{name, GateAvailable, "not configured: " + firstLine(err.Error())})
		return Candidate{}, false
	}
	if reason, marked := st.AccountAuthMarked(c.Account, now); marked {
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
func (t *RouteTable) gateCandidate(
	ctx context.Context, pg *pickGate, tier, runner string, pins map[string]PinSpec, st State, o PickOpts, now time.Time,
	configured func(runner string, c Cell) error, res *PickResult,
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
		if d, ok := t.gateDatabricksCodex(pg, tier, st, o, now, configured, res); ok && !hasProfileAccount(group, d.Account) {
			group = append(group, d)
		}
		if res.Want == "codex" {
			if _, model, effort, ok := t.codexHop(pg.reg, tier, o, now); ok {
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
func (t *RouteTable) gateSessionCopilot(
	ctx context.Context, pg *pickGate, tier, model, effort string, spec PinSpec, own []Candidate, st State, o PickOpts, now time.Time,
	configured func(runner string, c Cell) error, res *PickResult,
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
	profile, _, ok := sessionDatabricks(reg, "copilot", o.canServe)
	if !ok {
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

// sessionDatabricks is the session or pinned profile when it is a signed-in Databricks row agent can use.
func sessionDatabricks(reg *providers.Registry, agent string, canServe func(string, *providers.Provider) bool) (profile string, p *providers.Provider, ok bool) {
	if reg == nil {
		return "", nil, false
	}
	profile = cmp.Or(providers.SessionProfile(), providers.AgentRecord("claude").Profile)
	p = reg.Providers[profile]
	if p == nil || p.Preset != providers.PresetDatabricks || !canServe(agent, p) || !providers.CredentialPresent(p) {
		return "", nil, false
	}
	return profile, p, true
}

func hasProfileAccount(group []Candidate, account string) bool {
	for _, c := range group {
		if c.Account == account {
			return true
		}
	}
	return false
}
