package vendors

import (
	"context"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// Leg names a RouteInfo's resolved credential path. Why: Anthropic's terms forbid subscription OAuth
// in third-party tools, so neither chain below ever offers the Claude subscription (passthrough, no
// login) as a leg.
const (
	LegCopilotLogin  = "copilot-login"
	LegOwnLogin      = "own-login"
	LegByokAnthropic = "byok-anthropic"
	LegDatabricks    = "databricks"
	LegNone          = "none"
)

// RouteInfo is the vendor leg `claude-code configure github`, `generate` and `doctor` report: which
// credential path a vendor CLI runs on today, and why. OK is false only for LegNone.
type RouteInfo struct {
	Agent, Leg, Provider, Model, SmallModel, Reason string
	OK                                              bool
}

// DescribeRoute reports the fallback leg vendor (opencode|copilot) resolves to today, in priority
// order. Every other vendor reports LegNone with a plain explanation — they have no fallback chain.
func DescribeRoute(reg *providers.Registry, agentName string) RouteInfo {
	switch agentName {
	case "opencode":
		return describeOpenCodeRoute(reg)
	case "copilot":
		return describeCopilotRoute(reg)
	default:
		return RouteInfo{Agent: agentName, Leg: LegNone, Reason: "no fallback chain defined for " + agentName}
	}
}

// describeOpenCodeRoute: 1) opencode's own GitHub Copilot login; 2) the registry anthropic provider's
// key; 3) the registry gemini provider's key (Gemini free tier); 4) none.
func describeOpenCodeRoute(reg *providers.Registry) RouteInfo {
	if OpencodeOwnLoginPresent() {
		return RouteInfo{Agent: "opencode", Leg: LegOwnLogin, Provider: "opencode-go",
			Reason: "opencode's own opencode login (Go plan; OPENCODE_API_KEY from the vault when no stored login)", OK: true}
	}
	if OpencodeCopilotLoginPresent() {
		if p, ok := loginProvider(reg, "github", "copilot"); ok {
			if main, small, mok := OpencodeCopilotModelIDs(reg); mok {
				return RouteInfo{Agent: "opencode", Leg: LegCopilotLogin, Provider: p.Name, Model: main, SmallModel: small,
					Reason: "opencode's own GitHub Copilot login (opencode auth login)", OK: true}
			}
			return RouteInfo{Agent: "opencode", Leg: LegCopilotLogin, Provider: p.Name,
				Reason: "opencode's own GitHub Copilot login, but no sonnet/haiku model configured for github-copilot"}
		}
	}
	return RouteInfo{Agent: "opencode", Leg: LegNone, Reason: "no route for opencode: " + ConfigureHint("opencode")}
}

// describeCopilotRoute: 1) copilot's GitHub login on a workable pin; 2) the Databricks fallback; 3) BYOK with the
// registry anthropic provider's key; 4) none.
func describeCopilotRoute(reg *providers.Registry) RouteInfo {
	detail, workable := copilotHeadroom(context.Background(), CopilotPinLogin())
	if a, ok := copilotAuthSource(); ok && workable {
		if p, ok := loginProvider(reg, "github", "copilot"); ok {
			model, _ := providers.CopilotModel(reg)
			return RouteInfo{Agent: "copilot", Leg: LegCopilotLogin, Provider: p.Name, Model: model,
				Reason: "github-copilot (" + a.Label() + ")", OK: true}
		}
	}
	if ri := copilotDatabricksFallback(reg, detail); ri.OK {
		return ri
	}
	if p, ok := keyProvider(reg, "anthropic"); ok && providers.CredentialPresent(p) {
		return RouteInfo{Agent: "copilot", Leg: LegByokAnthropic, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "BYOK via ANTHROPIC_API_KEY", OK: true}
	}
	return RouteInfo{Agent: "copilot", Leg: LegNone, Reason: "no route for copilot: " + ConfigureHint("copilot")}
}

// routeTarget converts a resolved RouteInfo (OK and naming a registered provider) into a Target;
// ok is false for LegNone or a provider DescribeRoute named that the registry no longer has.
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
