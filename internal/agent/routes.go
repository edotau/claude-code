package agent

import (
	"errors"

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
