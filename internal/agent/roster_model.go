package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// ModelAsk is one agent-model decision, shared by the interactive launch and the headless dispatch.
type ModelAsk struct {
	Agent string
	// Flag is --model as typed: a model alias (opus) or a model id; "" = none.
	Flag string
	// Target is the provider and the model the agent runs with no flag and no pin.
	Target providers.Target
	// Applied means Target.Model already carries Flag (the session selection spelled it for Target.Provider).
	Applied bool
}

// AgentModelPin is the agent's own model pin (<AGENT>_MODEL, vault only), or "" when it has none or none is set.
func AgentModelPin(agent string) string {
	spec, ok := LookupSchema(agent)
	if !ok || spec.CLI.ModelPin == "" {
		return ""
	}
	return providers.Pin(spec.CLI.ModelPin)
}

// SpellPin is pin spelled the way p serves it, or "" when p cannot serve it (a Databricks wire id off Databricks).
func SpellPin(p *providers.Provider, pin string) string {
	if p == nil {
		return pin
	}
	if p.Preset != providers.PresetDatabricks && strings.HasPrefix(pin, models.DatabricksPrefix) {
		return ""
	}
	return p.WireModel(models.Strip1M(pin))
}

// ResolveAgentModel is the one rule for the model an agent runs: flag > the agent's model pin > Target.Model,
// spelled for Target.Provider. A model alias the provider lacks keeps Target.Model and says so in the note.
func ResolveAgentModel(a ModelAsk) (providers.Target, string) {
	t, p := a.Target, a.Target.Provider
	if a.Flag == "" {
		if pin := AgentModelPin(a.Agent); pin != "" {
			if m := SpellPin(p, pin); m != "" {
				t.Model = m
			}
		}
		return t, ""
	}
	if a.Applied {
		return t, ""
	}
	alias := slices.Contains(models.Slots, a.Flag)
	switch {
	case p == nil && !alias:
		t.Model = a.Flag
	case p == nil:
	case alias && p.Models[a.Flag] != "":
		t.Model = p.Models[a.Flag]
	case alias:
		return t, fmt.Sprintf("%s has no %s model; keeping %s", p.Name, a.Flag, t.Model)
	default:
		t.Model = p.WireModel(models.Strip1M(a.Flag))
	}
	return t, ""
}

// GeminiGatewayRow is a keyed Databricks row agy can reach through the router's native Gemini route.
func GeminiGatewayRow(p *providers.Provider) bool {
	return p != nil && p.Preset == providers.PresetDatabricks && p.Auth.Login == "" &&
		p.Auth.Method() != providers.AuthNone && p.Auth.Method() != providers.AuthCLI
}

// CanServe reports whether p is usable by agent name (Serves without the reason).
func CanServe(name string, p *providers.Provider) bool {
	if name == "gemini" && GeminiGatewayRow(p) {
		return true
	}
	ok, _ := Serves(name, p, "")
	return ok
}

// Serves reports whether p is usable by agent: a login provider naming it, or a route on one of the dialects it speaks.
// A non-empty model adds that the model's wire (WireFor) is one the agent speaks. why names the first thing missing.
func Serves(agent string, p *providers.Provider, model string) (ok bool, why string) {
	spec, known := LookupSchema(agent)
	if !known || !spec.External() {
		return false, fmt.Sprintf("unknown agent %q", agent)
	}
	// A cli-auth provider carries a login only its own agent can forward; a dialect match is not enough.
	if p.Auth.Login != "" {
		if p.Auth.Login == agent {
			return true, ""
		}
		return false, fmt.Sprintf("provider %s carries a %s login, which %s cannot use", p.Name, p.Auth.Login, agent)
	}
	if len(spec.CLI.Dialects) == 0 {
		return false, fmt.Sprintf("%s has no custom base URL; it runs only on its Google sign-in, not provider %s", spec.CLI.Binary, p.Name)
	}
	if !slices.ContainsFunc(spec.CLI.Dialects, func(d string) bool { _, ok := p.Route(d); return ok }) {
		return false, fmt.Sprintf("provider %s serves no %s route (it has %s)", p.Name,
			strings.Join(spec.CLI.Dialects, "/"), strings.Join(p.Dialects(), ","))
	}
	if p.Auth.Method() == providers.AuthCLI {
		return false, fmt.Sprintf("provider %s uses cli (subscription) auth, which %s cannot use; pick a keyed provider", p.Name, agent)
	}
	if model == "" {
		return true, ""
	}
	wire, ok, why := providers.WireFor(p, model)
	if !ok {
		return false, why
	}
	_, routed := spec.CLI.RouterSuffix[wire]
	if !routed && !slices.Contains(spec.CLI.Dialects, wire) {
		return false, fmt.Sprintf("%s does not speak the %s wire that provider %s uses for %s", agent, wire, p.Name, model)
	}
	return true, ""
}
