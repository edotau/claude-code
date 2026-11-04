package router

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
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

// codexHop is the model and effort codex's Databricks hops run for alias: codex's own pick respelled for the
// profile when it serves Responses, else the profile's first Responses model.
func (t *RouteTable) codexHop(reg *providers.Registry, alias string, o PickOpts, now time.Time) (profile, model, effort string, ok bool) {
	profile, first, ok := DatabricksResponses(reg, now, o.canServe)
	if !ok {
		return "", "", "", false
	}
	cell, _ := t.Cell("codex", alias)
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

// gateDatabricksCodex is codex's Databricks candidate at alias: the same configured/auth/exhausted gates as any
// agent, on account databricks·<profile>. ok is false (no Skip) when Databricks is off or serves no usable model.
func (t *RouteTable) gateDatabricksCodex(
	pg *pickGate, alias string, st State, o PickOpts, now time.Time, configured func(agent string, c Cell) error, res *PickResult,
) (Candidate, bool) {
	reg := pg.reg
	profile, model, effort, ok := t.codexHop(reg, alias, o, now)
	if !ok {
		return Candidate{}, false
	}
	c := Cell{Model: model, Effort: effort, Account: ProviderAccount(profile)}
	name := "codex·" + c.Account
	if err := configured("codex", c); err != nil {
		res.Skipped = append(res.Skipped, Skip{name, GateAvailable, "not configured: " + textutil.FirstLine(strings.TrimSpace(err.Error()))})
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

// gateCandidate gates one agent (off, pinned or unpinned); codex and copilot also offer candidates on the session profile.
func (t *RouteTable) gateCandidate(
	ctx context.Context, pg *pickGate, alias, agent string, pins map[string]PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) ([]Candidate, bool) {
	spec := pins[agent]
	if spec.Off {
		res.Skipped = append(res.Skipped, Skip{agent, GateOff, "deactivated (" + strings.ToUpper(agent) + "_ACTIVE=off)"})
		return nil, false
	}
	var group []Candidate
	if agent != "claude" && spec.Profile != "" {
		group, _ = t.gatePinned(ctx, pg.reg, alias, agent, spec.Profile, spec, st, o, now, configured, res)
	} else {
		group, _ = t.gateAgent(ctx, alias, agent, st, o, now, configured, res)
		if agent == "gemini" && spec.Model != "" { // native agy login: the GEMINI_MODEL pin beats the table cell
			for i := range group {
				group[i].Model = spec.Model
			}
		}
	}
	if agent == "copilot" {
		group = append(group, t.gateSessionCopilot(ctx, pg, alias, "", "", spec, group, st, o, now, configured, res)...)
	}
	if agent == "codex" && spec.Profile == "" {
		group = append(group, t.gateCodexExtras(ctx, pg, alias, group, pins["copilot"], st, o, now, configured, res)...)
	}
	return group, len(group) > 0
}

// gateCodexExtras adds the session Databricks codex candidate and, when codex is wanted, the copilot hop.
func (t *RouteTable) gateCodexExtras(
	ctx context.Context, pg *pickGate, alias string, group []Candidate, copilotSpec PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	var extra []Candidate
	if d, ok := t.gateDatabricksCodex(pg, alias, st, o, now, configured, res); ok && !hasProfileAccount(group, d.Account) {
		extra = append(extra, d)
	}
	if res.Want != "codex" {
		return extra
	}
	if _, model, effort, ok := t.codexHop(pg.reg, alias, o, now); ok {
		hop := t.gateSessionCopilot(ctx, pg, alias, model, effort, copilotSpec, nil, st, o, now, configured, res)
		for _, c := range hop {
			pg.held[c.Account] = true
		}
		extra = append(extra, hop...)
	}
	return extra
}

// gateSessionCopilot checks copilot on the session's Databricks profile.
func (t *RouteTable) gateSessionCopilot(
	ctx context.Context, pg *pickGate, alias, model, effort string, spec PinSpec, own []Candidate, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	reg := pg.reg
	if spec.Off || reg == nil || !copilotPinOK(reg, spec) {
		return nil
	}
	profile, _, ok := sessionDatabricks(reg, "copilot", o.canServe)
	if !ok {
		return nil
	}
	account := ProviderAccount(profile)
	if hasProfileAccount(own, account) || (own == nil && t.sessionCopilotTaken(pg, alias, account, model, spec, o)) {
		return nil
	}
	group, _ := t.gateProfile(ctx, reg, alias, "copilot", profile, model, effort, st, o, now, configured, res)
	return group
}

// sessionCopilotTaken reports that account is already held by an earlier gate or is the copilot cell's own account.
func (t *RouteTable) sessionCopilotTaken(pg *pickGate, alias, account, model string, spec PinSpec, o PickOpts) bool {
	return (model == "" && pg.held[account]) || t.copilotCellOn(alias, account, spec, o)
}

// copilotPinOK is true when spec pins no profile, or pins a github one.
func copilotPinOK(reg *providers.Registry, spec PinSpec) bool {
	if spec.Profile == "" {
		return true
	}
	p := reg.Providers[spec.Profile]
	return p != nil && p.Provider == "github"
}

// copilotCellOn reports whether the table's own copilot cell for alias already resolves to account.
func (t *RouteTable) copilotCellOn(alias, account string, spec PinSpec, o PickOpts) bool {
	cell, ok := t.Cell("copilot", alias)
	if !ok {
		return false
	}
	if spec.Profile != "" {
		cell.Account = ProviderAccount(spec.Profile)
	} else if o.Account != nil {
		if a, _ := o.Account("copilot", cell); a != "" {
			cell.Account = a
		}
	}
	return cell.Account == account
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
