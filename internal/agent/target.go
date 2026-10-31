package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// ResolveTarget picks the target an agent launch uses: sel's target when it can serve the agent
// (a login provider naming it, or a route on one of its dialects), else — only when the caller named
// no provider itself (explicit is false) — the agent's own subscription login or first keyed
// provider with a resolvable credential. note is a one-line explanation to print on fallback.
func (s Schema) ResolveTarget(reg *providers.Registry, sel providers.Selection, explicit bool) (target providers.Target, note string, err error) {
	name, e := s.Name, s.eng()
	t, ok := sel.Slots[models.Opus]
	if !ok {
		return providers.Target{}, "", fmt.Errorf("%s: no opus target resolved", name)
	}
	// "OpenCode defaults to the Copilot plan": a CLI's own login (unrelated to the session provider) is preferred
	// whenever it is present, ahead of a session provider that could serve it; an explicit --provider still wins.
	if pt, pnote, ok, perr := e.preRoute(reg, explicit); ok {
		return pt, pnote, perr
	}
	if explicit || (CanServe(name, t.Provider) && e.followsPin(t.Provider)) {
		return e.gate(reg, t)
	}
	if ft, fallbackNote, ok := e.fallbackTarget(reg); ok {
		note = fmt.Sprintf("%s: session provider %s cannot serve %s; using %s", name, t.Provider.Name, name, fallbackNote)
		if own, ok := e.fallbackNote(ft, fallbackNote); ok {
			note = own
		}
		if ft.Provider == nil {
			return ft, note, ErrOwnLogin // the agent's own stored login: launch it plain, nothing to inject
		}
		return ft, note, nil
	}
	// opencode and copilot have a defined fallback chain (DescribeRoute); nothing in it resolved either.
	if err := e.noRoute(reg, t); err != nil {
		return providers.Target{}, "", err
	}
	return t, "", nil
}

// copilotGate keeps t unless it is copilot's GitHub login on an unworkable pin; then the Databricks fallback and its
// notice — never another GitHub account.
func copilotGate(reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	if t.Provider == nil || t.Provider.Auth.Login != "copilot" {
		return t, "", nil
	}
	detail, ok := copilotHeadroom(context.Background(), copilotPinLogin())
	if ok {
		return t, "", nil
	}
	return copilotFallback(reg, detail)
}

// copilotFallback is the copilot Databricks fallback target and its notice; detail names why GitHub was refused.
func copilotFallback(reg *providers.Registry, detail string) (providers.Target, string, error) {
	ri := copilotDatabricksFallback(reg, detail)
	ft, ok := routeTarget(reg, ri)
	if !ok {
		return providers.Target{}, "", errors.New(ri.Reason)
	}
	return ft, ri.Reason, nil
}

// copilotDatabricksFallback: this session's (else the vault-pinned) profile when it is a signed-in Databricks row,
// else the first Databricks profile whose credential resolves, else LegNone. detail names why GitHub was refused.
func copilotDatabricksFallback(reg *providers.Registry, detail string) RouteInfo {
	pick := func(p *providers.Provider) RouteInfo {
		return RouteInfo{Agent: "copilot", Leg: LegDatabricks, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "copilot: " + detail + " — using databricks profile " + p.Name, OK: true}
	}
	pin := cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile))
	for _, name := range append([]string{pin}, providers.DatabricksProfiles(reg)...) {
		if p := reg.Providers[name]; databricksUsable(p) {
			return pick(p)
		}
	}
	return RouteInfo{Agent: "copilot", Leg: LegNone,
		Reason: "copilot: " + detail + "; no credit-free route for copilot (no Databricks profile signed in)"}
}

// databricksUsable is true for a Databricks row whose credential resolves.
func databricksUsable(p *providers.Provider) bool {
	if p == nil || p.Preset != providers.PresetDatabricks {
		return false
	}
	_, err := providers.Credential(context.Background(), p)
	return providers.Verdict(err) == providers.VerdictOK
}

// ErrOwnLogin means the agent should run on the credentials its own CLI stores (no harness route injected).
var ErrOwnLogin = errors.New("agent runs on its own login")

// keyProvider is the first profile under provider that bills on a harness-held key.
func keyProvider(reg *providers.Registry, provider string) (*providers.Provider, bool) {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.APIKeyAuth() {
			return p, true
		}
	}
	return nil, false
}

// loginProvider is the first profile under provider whose login serves agent.
func loginProvider(reg *providers.Registry, provider, name string) (*providers.Provider, bool) {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.Auth.Login == name {
			return p, true
		}
	}
	return nil, false
}
