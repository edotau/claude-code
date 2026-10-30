package vendors

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/copilotreg"
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
	spec, ok := providers.LookupAgentSpec(agent)
	if !ok || spec.ModelPin == "" {
		return ""
	}
	return providers.Pin(spec.ModelPin)
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

// CopilotChoice is the copilot registry's model choice: the requested models, else the agent's model pin.
func CopilotChoice(requested []string) copilotreg.Choice {
	return copilotreg.Choice{Requested: requested, Pin: AgentModelPin("copilot")}
}
