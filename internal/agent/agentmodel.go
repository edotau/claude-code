package agent

import ()

// copilotChoice is the copilot registry's model choice: the requested models, else the agent's model pin.
func copilotChoice(requested []string) CopilotChoice {
	return CopilotChoice{Requested: requested, Pin: AgentModelPin("copilot")}
}
