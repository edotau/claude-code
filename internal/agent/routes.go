package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// Dispatch names a RouteInfo's resolved credential path. Why: Anthropic's terms forbid subscription OAuth
// in third-party tools, so neither chain below ever offers the Claude subscription (passthrough, no
// login) as a dispatch.
const (
	RouteCopilotLogin  = "copilot-login"
	RouteOwnLogin      = "own-login"
	RouteByokAnthropic = "byok-anthropic"
	RouteDatabricks    = "databricks"
	RouteNone          = "none"
)

// RouteInfo is the agent dispatch `claude-code configure github`, `generate` and `doctor` report: which
// credential path an agent CLI runs on today, and why. OK is false only for RouteNone.
type RouteInfo struct {
	Agent, Route, Provider, Model, SmallModel, Reason string
	OK                                                bool
}

// DescribeRoute reports the fallback dispatch the agent (opencode|copilot) resolves to today, in priority
// order. Every other agent reports RouteNone with a plain explanation — they have no fallback chain.
func (s Schema) DescribeRoute(reg *providers.Registry) RouteInfo { return s.eng().DescribeRoute(reg) }

// RouteTarget converts a resolved RouteInfo (OK and naming a registered provider) into a Target;
// ok is false for RouteNone or a provider DescribeRoute named that the registry no longer has.
func RouteTarget(reg *providers.Registry, ri RouteInfo) (providers.Target, bool) {
	if !ri.OK || ri.Provider == "" {
		return providers.Target{}, false
	}
	p, ok := reg.Providers[ri.Provider]
	if !ok {
		return providers.Target{}, false
	}
	return providers.Target{Provider: p, Model: ri.Model}, true
}

// RouteErr is ri's reason as an error when ri resolved no route, else nil.
func RouteErr(ri RouteInfo) error {
	if !ri.OK {
		return errors.New(ri.Reason)
	}
	return nil
}

// RouteDetail returns the "<agent> route" detail string DoctorChecks would emit for the agent, without
// running the rest of the doctor sweep; ok is false for an unknown agent or a registry load error.
func RouteDetail(name string) (detail string, ok bool) {
	return schemaFor(name).eng().RouteDetail()
}

// LoadSelection is the registry and its unpinned selection; ok is false when the registry does not load.
func LoadSelection() (reg *providers.Registry, sel providers.Selection, selErr error, ok bool) {
	reg, err := providers.Load()
	if err != nil || reg == nil {
		return nil, providers.Selection{}, nil, false
	}
	sel, selErr = providers.Select(reg, "")
	return reg, sel, selErr, true
}

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
