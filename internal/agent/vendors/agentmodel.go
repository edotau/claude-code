package vendors

import (
	"github.com/edotau/claude-code/internal/agent/copilotreg"
	"github.com/edotau/claude-code/internal/providers"
)

// CopilotChoice is the copilot registry's model choice: the requested models, else the agent's model pin.
func CopilotChoice(requested []string) copilotreg.Choice {
	return copilotreg.Choice{Requested: requested, Pin: providers.AgentModelPin("copilot")}
}
