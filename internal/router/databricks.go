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

// gateCandidate gates one runner (off, pinned or unpinned); codex and copilot also offer candidates on the session profile.
func (t *Table) gateCandidate(
	ctx context.Context, tier, runner string, pins map[string]providers.AgentPinSpec, st State, o Opts, now time.Time,
	configured func(runner string, c Cell) error, res *Result,
) ([]Candidate, bool) {
	spec := pins[runner]
	if spec.Off {
		res.Skipped = append(res.Skipped, Skip{runner, GateOff, "deactivated (" + strings.ToUpper(runner) + "_ACTIVE=off)"})
		return nil, false
	}
	var group []Candidate
	if runner != "claude" && spec.Profile != "" {
		group, _ = t.gatePinned(ctx, tier, runner, spec.Profile, spec, st, o, now, configured, res)
	} else {
		group, _ = t.gateRunner(ctx, tier, runner, st, o, now, configured, res)
	}
	if runner == "copilot" {
		group = append(group, t.gateSessionCopilot(ctx, tier, "", spec, group, st, o, now, configured, res)...)
	}
	if runner == "codex" && spec.Profile == "" {
		if d, ok := t.gateDatabricksCodex(st, now, configured, res); ok && !hasProfileAccount(group, d.Account) {
			group = append(group, d)
		}
		if res.Want == "codex" {
			reg, _ := providers.Load()
			if _, model, ok := DatabricksResponses(reg); ok {
				group = append(group, t.gateSessionCopilot(ctx, tier, model, pins["copilot"], nil, st, o, now, configured, res)...)
			}
		}
	}
	return group, len(group) > 0
}

// gateSessionCopilot checks copilot on the session's Databricks profile.
func (t *Table) gateSessionCopilot(
	ctx context.Context, tier, model string, spec providers.AgentPinSpec, own []Candidate, st State, o Opts, now time.Time,
	configured func(runner string, c Cell) error, res *Result,
) []Candidate {
	if spec.Off {
		return nil
	}
	reg, _ := providers.Load()
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
	if hasProfileAccount(own, account) {
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
	group, _ := t.gateProfile(ctx, tier, "copilot", profile, model, st, o, now, configured, res)
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
