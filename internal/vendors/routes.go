package vendors

import (
	"fmt"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// Leg names a RouteInfo's resolved credential path. Why: Anthropic's terms forbid subscription OAuth
// in third-party tools, so neither chain below ever offers the Claude subscription (passthrough, no
// login) as a leg.
const (
	LegCopilotLogin  = "copilot-login"
	LegAnthropicKey  = "anthropic-key"
	LegGeminiFree    = "gemini-free"
	LegByokAnthropic = "byok-anthropic"
	LegByokGemini    = "byok-gemini"
	LegNone          = "none"
)

// RouteInfo is the vendor leg `claude-code configure github`, `generate` and `doctor` report: which
// credential path a vendor CLI runs on today, and why. OK is false only for LegNone.
type RouteInfo struct {
	Vendor, Leg, Provider, Model, SmallModel, Reason string
	OK                                               bool
}

// DescribeRoute reports the fallback leg vendor (opencode|copilot) resolves to today, in priority
// order. Every other vendor reports LegNone with a plain explanation — they have no fallback chain.
func DescribeRoute(reg *providers.Registry, vendor string) RouteInfo {
	switch vendor {
	case "opencode":
		return describeOpenCodeRoute(reg)
	case "copilot":
		return describeCopilotRoute(reg)
	default:
		return RouteInfo{Vendor: vendor, Leg: LegNone, Reason: "no fallback chain defined for " + vendor}
	}
}

// describeOpenCodeRoute: 1) opencode's own GitHub Copilot login; 2) the registry anthropic provider's
// key; 3) the registry gemini provider's key (Gemini free tier); 4) none.
func describeOpenCodeRoute(reg *providers.Registry) RouteInfo {
	if OpencodeCopilotLoginPresent() {
		if p, ok := loginProvider(reg, "github-copilot", "copilot"); ok {
			if main, small, mok := OpencodeCopilotModelIDs(reg); mok {
				return RouteInfo{Vendor: "opencode", Leg: LegCopilotLogin, Provider: p.Name, Model: main, SmallModel: small,
					Reason: "opencode's own GitHub Copilot login (opencode auth login)", OK: true}
			}
			return RouteInfo{Vendor: "opencode", Leg: LegCopilotLogin, Provider: p.Name,
				Reason: "opencode's own GitHub Copilot login, but no sonnet/haiku model configured for github-copilot"}
		}
	}
	if p, ok := reg.Providers["anthropic"]; ok && providers.CredentialPresent(p) {
		return RouteInfo{Vendor: "opencode", Leg: LegAnthropicKey, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "ANTHROPIC_API_KEY resolves", OK: true}
	}
	if p, ok := reg.Providers["gemini"]; ok && providers.CredentialPresent(p) {
		return RouteInfo{Vendor: "opencode", Leg: LegGeminiFree, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "GEMINI_API_KEY resolves (Gemini free tier)", OK: true}
	}
	return RouteInfo{Vendor: "opencode", Leg: LegNone, Reason: fmt.Sprintf("no route for opencode: %s", ConfigureHint("opencode"))}
}

// describeCopilotRoute: 1) copilot's own GitHub login; 2) BYOK with the registry anthropic provider's
// key; 3) BYOK with the registry gemini provider's key; 4) none.
func describeCopilotRoute(reg *providers.Registry) RouteInfo {
	if copilotLoginPresent() {
		if p, ok := loginProvider(reg, "github-copilot", "copilot"); ok {
			return RouteInfo{Vendor: "copilot", Leg: LegCopilotLogin, Provider: p.Name, Model: p.Models[models.Opus],
				Reason: "github-copilot (GH_TOKEN/gh auth)", OK: true}
		}
	}
	if p, ok := reg.Providers["anthropic"]; ok && providers.CredentialPresent(p) {
		return RouteInfo{Vendor: "copilot", Leg: LegByokAnthropic, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "BYOK via ANTHROPIC_API_KEY", OK: true}
	}
	if p, ok := reg.Providers["gemini"]; ok && providers.CredentialPresent(p) {
		return RouteInfo{Vendor: "copilot", Leg: LegByokGemini, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "BYOK via GEMINI_API_KEY", OK: true}
	}
	return RouteInfo{Vendor: "copilot", Leg: LegNone, Reason: fmt.Sprintf("no route for copilot: %s", ConfigureHint("copilot"))}
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
