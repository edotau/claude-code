package agent

import (
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
	t, ok := sel.Aliases[models.Opus]
	if !ok {
		return providers.Target{}, "", fmt.Errorf("%s: no opus target resolved", name)
	}
	// A CLI's own login (unrelated to the session provider) is preferred
	// whenever it is present, ahead of a session provider that could serve it; an explicit --provider still wins.
	if pt, pnote, ok, perr := e.PreRoute(reg, explicit); ok {
		return pt, pnote, perr
	}
	if explicit || (CanServe(name, t.Provider) && e.FollowsPin(t.Provider)) {
		return e.Gate(reg, t)
	}
	if ft, fallbackNote, ok := e.FallbackTarget(reg); ok {
		note = fmt.Sprintf("%s: session provider %s cannot serve %s; using %s", name, t.Provider.Name, name, fallbackNote)
		if own, ok := e.FallbackNote(ft, fallbackNote); ok {
			note = own
		}
		if ft.Provider == nil {
			return ft, note, ErrOwnLogin // the agent's own stored login: launch it plain, nothing to inject
		}
		return ft, note, nil
	}
	// opencode and copilot have a defined fallback chain (DescribeRoute); nothing in it resolved either.
	if err := e.NoRoute(reg, t); err != nil {
		return providers.Target{}, "", err
	}
	return t, "", nil
}

// DatabricksUsable is true for a Databricks row whose credential resolves.
func DatabricksUsable(p *providers.Provider) bool {
	if p == nil || p.Preset != providers.PresetDatabricks {
		return false
	}
	_, err := providers.Credential(context.Background(), p)
	return providers.Verdict(err) == providers.VerdictOK
}

// ErrOwnLogin means the agent should run on the credentials its own CLI stores (no harness route injected).
var ErrOwnLogin = errors.New("agent runs on its own login")

// KeyProvider is the first profile under provider that bills on a harness-held key.
func KeyProvider(reg *providers.Registry, provider string) (*providers.Provider, bool) {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.APIKeyAuth() {
			return p, true
		}
	}
	return nil, false
}

// LoginProvider is the first profile under provider whose login serves agent.
func LoginProvider(reg *providers.Registry, provider, name string) (*providers.Provider, bool) {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.Auth.Login == name {
			return p, true
		}
	}
	return nil, false
}
