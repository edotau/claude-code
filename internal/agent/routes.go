package agent

import (
	"context"

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
func (s Schema) DescribeRoute(reg *providers.Registry) RouteInfo { return s.eng().describeRoute(reg) }

// describeOpenCodeRoute: 1) opencode's own login (Go plan); 2) opencode's GitHub Copilot login; 3) none.
func describeOpenCodeRoute(reg *providers.Registry) RouteInfo {
	if OpencodeOwnLoginPresent() {
		return RouteInfo{Agent: "opencode", Route: RouteOwnLogin, Provider: "opencode-go",
			Reason: "opencode's own opencode login (Go plan; OPENCODE_API_KEY from the vault when no stored login)", OK: true}
	}
	if OpencodeCopilotLoginPresent() {
		if p, ok := loginProvider(reg, "github", "copilot"); ok {
			if main, small, mok := OpencodeCopilotModelIDs(reg); mok {
				return RouteInfo{Agent: "opencode", Route: RouteCopilotLogin, Provider: p.Name, Model: main, SmallModel: small,
					Reason: "opencode's own GitHub Copilot login (opencode auth login)", OK: true}
			}
			return RouteInfo{Agent: "opencode", Route: RouteCopilotLogin, Provider: p.Name,
				Reason: "opencode's own GitHub Copilot login, but no sonnet/haiku model configured for github-copilot"}
		}
	}
	s, _ := LookupSchema("opencode")
	return RouteInfo{Agent: "opencode", Route: RouteNone, Reason: "no route for opencode: " + s.CLI.ConfigureHint}
}

// describeCopilotRoute: 1) copilot's GitHub login on a workable pin; 2) the Databricks fallback; 3) BYOK with the
// registry anthropic provider's key; 4) none.
func describeCopilotRoute(reg *providers.Registry) RouteInfo {
	if pin := providers.Pin(providers.PinCopilotProfile); reg != nil && databricksUsable(reg.Providers[pin]) { // a pinned Databricks profile is the route
		p := reg.Providers[pin]
		t, _ := ResolveAgentModel(ModelAsk{Agent: "copilot", Target: providers.Target{Provider: p, Model: p.Models[models.Opus]}})
		return RouteInfo{Agent: "copilot", Route: RouteDatabricks, Provider: p.Name, Model: t.Model,
			Reason: "copilot: pinned databricks profile " + p.Name, OK: true}
	}
	detail, workable := copilotHeadroom(context.Background(), copilotPinLogin())
	if a, ok := copilotAuthSource(); ok && workable {
		if p, ok := loginProvider(reg, "github", "copilot"); ok {
			model, _ := providers.CopilotModel(reg)
			return RouteInfo{Agent: "copilot", Route: RouteCopilotLogin, Provider: p.Name, Model: model,
				Reason: "github-copilot (" + a.Label() + ")", OK: true}
		}
	}
	if ri := copilotDatabricksFallback(reg, detail); ri.OK {
		return ri
	}
	if p, ok := keyProvider(reg, "anthropic"); ok && providers.CredentialPresent(p) {
		return RouteInfo{Agent: "copilot", Route: RouteByokAnthropic, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "BYOK via ANTHROPIC_API_KEY", OK: true}
	}
	s, _ := LookupSchema("copilot")
	return RouteInfo{Agent: "copilot", Route: RouteNone, Reason: "no route for copilot: " + s.CLI.ConfigureHint}
}

// routeTarget converts a resolved RouteInfo (OK and naming a registered provider) into a Target;
// ok is false for RouteNone or a provider DescribeRoute named that the registry no longer has.
func routeTarget(reg *providers.Registry, ri RouteInfo) (providers.Target, bool) {
	if !ri.OK || ri.Provider == "" {
		return providers.Target{}, false
	}
	p, ok := reg.Providers[ri.Provider]
	if !ok {
		return providers.Target{}, false
	}
	return providers.Target{Provider: p, Model: ri.Model}, true
}
