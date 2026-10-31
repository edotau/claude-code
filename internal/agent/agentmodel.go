package agent

import (
	"github.com/edotau/claude-code/internal/providers"
)

// copilotChoice is the copilot registry's model choice: the requested models, else the agent's model pin.
func copilotChoice(requested []string) CopilotChoice {
	return CopilotChoice{Requested: requested, Pin: providers.AgentModelPin("copilot")}
}
