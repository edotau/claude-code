package router

import (
	"cmp"
	"context"
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

// gateDatabricksCodex is codex's Databricks candidate at tier: the same configured/auth/exhausted gates as any
// runner, on account databricks·<profile>. ok is false (no Skip) when Databricks is off or serves no usable model.
func (t *Table) gateDatabricksCodex(
	st State, now time.Time, configured func(runner string, c Cell) error, res *Result,
) (Candidate, bool) {
	reg, _ := providers.Load()
	profile, model, ok := DatabricksResponses(reg)
	if !ok {
		return Candidate{}, false
	}
	c := Cell{Model: model, Account: ProviderAccount(profile)}
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

// gateCandidate gates one runner; codex also offers its Databricks candidate right after the ChatGPT one.
func (t *Table) gateCandidate(
	ctx context.Context, tier, runner string, st State, o Opts, now time.Time,
	configured func(runner string, c Cell) error, res *Result,
) ([]Candidate, bool) {
	group, ok := t.gateRunner(ctx, tier, runner, st, o, now, configured, res)
	if runner != "codex" {
		return group, ok
	}
	if d, dok := t.gateDatabricksCodex(st, now, configured, res); dok {
		return append(group, d), true
	}
	return group, ok
}
