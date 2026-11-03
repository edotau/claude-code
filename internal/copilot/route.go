package copilot

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

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
	ft, ok := agent.RouteTarget(reg, ri)
	if !ok {
		return providers.Target{}, "", errors.New(ri.Reason)
	}
	return ft, ri.Reason, nil
}

// copilotDatabricksFallback: this session's (else the vault-pinned) profile when it is a signed-in Databricks row,
// else the first Databricks profile whose credential resolves, else RouteNone. detail names why GitHub was refused.
func copilotDatabricksFallback(reg *providers.Registry, detail string) agent.RouteInfo {
	pick := func(p *providers.Provider) agent.RouteInfo {
		return agent.RouteInfo{Agent: "copilot", Route: agent.RouteDatabricks, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "copilot: " + detail + " — using databricks profile " + p.Name, OK: true}
	}
	pin := cmp.Or(providers.SessionProfile(), vault.GetAgentRecord("claude").Profile)
	for _, name := range append([]string{pin}, providers.DatabricksProfiles(reg)...) {
		if p := reg.Providers[name]; agent.DatabricksUsable(p) {
			return pick(p)
		}
	}
	return agent.RouteInfo{Agent: "copilot", Route: agent.RouteNone,
		Reason: "copilot: " + detail + "; no credit-free route for copilot (no Databricks profile signed in)"}
}

// describeCopilotRoute: 1) copilot's GitHub login on a workable pin; 2) the Databricks fallback; 3) BYOK with the
// registry anthropic provider's key; 4) none.
func describeCopilotRoute(reg *providers.Registry) agent.RouteInfo {
	if pin := vault.GetAgentRecord("copilot").Profile; reg != nil && agent.DatabricksUsable(reg.Providers[pin]) { // a pinned Databricks profile is the route
		p := reg.Providers[pin]
		t, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: "copilot", Target: providers.Target{Provider: p, Model: p.Models[models.Opus]}})
		return agent.RouteInfo{Agent: "copilot", Route: agent.RouteDatabricks, Provider: p.Name, Model: t.Model,
			Reason: "copilot: pinned databricks profile " + p.Name, OK: true}
	}
	detail, workable := copilotHeadroom(context.Background(), copilotPinLogin())
	if a, ok := copilotAuthSource(); ok && workable {
		if p, ok := agent.LoginProvider(reg, "github", "copilot"); ok {
			model, _ := providers.CopilotModel(reg)
			return agent.RouteInfo{Agent: "copilot", Route: agent.RouteCopilotLogin, Provider: p.Name, Model: model,
				Reason: "github-copilot (" + providers.AuthState{Account: a.Account, Source: a.Source}.Label() + ")", OK: true}
		}
	}
	if ri := copilotDatabricksFallback(reg, detail); ri.OK {
		return ri
	}
	if p, ok := agent.KeyProvider(reg, "anthropic"); ok && providers.CredentialPresent(p) {
		return agent.RouteInfo{Agent: "copilot", Route: agent.RouteByokAnthropic, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "BYOK via ANTHROPIC_API_KEY", OK: true}
	}
	s, _ := agent.LookupSchema("copilot")
	return agent.RouteInfo{Agent: "copilot", Route: agent.RouteNone, Reason: "no route for copilot: " + s.CLI.ConfigureHint}
}

// copilotRouteDetail reports the copilot CLI's resolved dispatch (DescribeRoute): its own GitHub login,
// BYOK with an anthropic or gemini key, or none — names/booleans/dispatch only, never a credential. On the
// GitHub login dispatch it also names COPILOT_MODEL's source and the effort/context pins every launch reads.
func copilotRouteDetail(reg *providers.Registry, selErr error) string {
	if selErr != nil {
		return fmt.Sprintf("(%v)", selErr)
	}
	ri := copilotSchema().DescribeRoute(reg)
	if !ri.OK {
		return ri.Reason
	}
	detail := fmt.Sprintf("dispatch=%s provider=%s model=%s (%s)", ri.Route, ri.Provider, ri.Model, ri.Reason)
	if ri.Route == agent.RouteCopilotLogin {
		_, source := providers.CopilotModel(reg)
		rec := vault.GetAgentRecord("copilot")
		effort, context := cmp.Or(rec.Effort, "default"), cmp.Or(rec.Context, "default")
		detail += fmt.Sprintf(" source=%s effort=%s context=%s", source, effort, context)
	}
	return detail
}

// RouteStep is one link of copilot's route chain: what it runs on, when it applies, and whether it is usable now.
type RouteStep struct {
	Label, When string
	OK          bool
}

// RouteChain is copilot's routes in describeCopilotRoute's try order, offline (no credit lookup).
func RouteChain(reg *providers.Registry) []RouteStep {
	if reg == nil {
		return nil
	}
	var steps []RouteStep
	pin := vault.GetAgentRecord("copilot").Profile
	if p := reg.Providers[pin]; p != nil && p.Preset == providers.PresetDatabricks {
		steps = append(steps, RouteStep{Label: "databricks-" + pin, When: "pinned", OK: agent.DatabricksUsable(p)})
	}
	a, ok := copilotAuthSource()
	gh := "GitHub"
	if a.Account != "" {
		gh += " " + a.Account
	}
	steps = append(steps, RouteStep{Label: gh, When: fmt.Sprintf("under %.0f%% of credits", copilotWorkablePct), OK: ok})
	for _, name := range append([]string{cmp.Or(providers.SessionProfile(), vault.GetAgentRecord("claude").Profile)},
		providers.DatabricksProfiles(reg)...) {
		if p := reg.Providers[name]; name != pin && agent.DatabricksUsable(p) {
			steps = append(steps, RouteStep{Label: "databricks-" + name, When: "fallback", OK: true})
			break
		}
	}
	if p, ok := agent.KeyProvider(reg, "anthropic"); ok {
		steps = append(steps, RouteStep{Label: "ANTHROPIC_API_KEY", When: "BYOK", OK: providers.CredentialPresent(p)})
	}
	return steps
}
