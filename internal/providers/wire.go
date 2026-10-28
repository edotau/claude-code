package providers

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// WireFor is the one rule for which wire model uses on p, from p's routes and the catalog: a Claude model takes the
// anthropic route; a Responses model the responses route; anything else the openai route. ok is false, with why, when p
// has no such route.
func WireFor(p *Provider, model string) (wire string, ok bool, why string) {
	has := func(d string) bool { _, ok := p.Route(d); return ok }
	claude := models.FamilyKind(model) == models.FamilyClaude
	switch {
	case claude && has(ProtocolAnthropic):
		return ProtocolAnthropic, true, ""
	case !claude && ServesResponses(p, model):
		if has(ProtocolResponses) {
			return ProtocolResponses, true, ""
		}
		return "", false, fmt.Sprintf("%s is Responses-only and provider %s has no responses route", model, p.Name)
	case has(ProtocolOpenAI):
		return ProtocolOpenAI, true, ""
	}
	return "", false, fmt.Sprintf("provider %s has no openai route to serve %s (it has %s)", p.Name, model, strings.Join(p.Dialects(), ","))
}

// Serves reports whether p is usable by agent: a login provider naming it, or a route on one of the dialects it speaks.
// A non-empty model adds that the model's wire (WireFor) is one the agent speaks. why names the first thing missing.
func Serves(agent string, p *Provider, model string) (ok bool, why string) {
	spec, known := LookupAgentSpec(agent)
	if !known || spec.Binary == "" {
		return false, fmt.Sprintf("unknown agent %q", agent)
	}
	// A cli-auth provider carries a login only its own agent can forward; a dialect match is not enough.
	if p.Auth.Login != "" {
		if p.Auth.Login == agent {
			return true, ""
		}
		return false, fmt.Sprintf("provider %s carries a %s login, which %s cannot use", p.Name, p.Auth.Login, agent)
	}
	if len(spec.Dialects) == 0 {
		return false, fmt.Sprintf("%s has no custom base URL; it runs only on its Google sign-in, not provider %s", spec.Binary, p.Name)
	}
	if !slices.ContainsFunc(spec.Dialects, func(d string) bool { _, ok := p.Route(d); return ok }) {
		return false, fmt.Sprintf("provider %s serves no %s route (it has %s)", p.Name,
			strings.Join(spec.Dialects, "/"), strings.Join(p.Dialects(), ","))
	}
	if p.Auth.Method() == AuthCLI {
		return false, fmt.Sprintf("provider %s uses cli (subscription) auth, which %s cannot use; pick a keyed provider", p.Name, agent)
	}
	if model == "" {
		return true, ""
	}
	wire, ok, why := WireFor(p, model)
	if !ok {
		return false, why
	}
	_, routed := spec.RouterSuffix[wire]
	if !routed && !slices.Contains(spec.Dialects, wire) {
		return false, fmt.Sprintf("%s does not speak the %s wire that provider %s uses for %s", agent, wire, p.Name, model)
	}
	return true, ""
}

// ClaudeCodeError is non-nil when Claude Code cannot run model on p: a Responses-only model has no translate path.
func ClaudeCodeError(p *Provider, model string) error {
	if wire, ok, _ := WireFor(p, model); ok && wire == ProtocolResponses {
		return fmt.Errorf("model %s is Responses-only and the router has no Responses translation for Claude Code (provider %s); run it with codex or copilot", model, p.Name)
	}
	return nil
}

// AnthropicRoute is the anthropic route model rides on p, or false: WireFor picked it, or the model is unservable on a
// non-Databricks row (third-party hosts serve their own models there; Databricks' anthropic route is Claude-only).
func AnthropicRoute(p *Provider, model string) (string, bool) {
	route, has := p.Route(ProtocolAnthropic)
	if !has {
		return "", false
	}
	wire, ok, _ := WireFor(p, model)
	if wire == ProtocolAnthropic || !ok && p.Preset != PresetDatabricks {
		return route, true
	}
	return "", false
}
