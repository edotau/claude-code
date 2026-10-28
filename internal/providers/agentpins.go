package providers

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// AgentOn and AgentOff are the <AGENT>_ACTIVE pin values; unset means on.
const (
	AgentOn  = "on"
	AgentOff = "off"
)

// AgentPinSpec is one agent's routing pins: its profile, its model pin, and whether it is switched off.
type AgentPinSpec struct {
	Profile, Model string
	Off            bool
}

// agentPinKeys maps each agent to its profile, model and active pin keys; opencode has no model pin.
var agentPinKeys = map[string]struct{ profile, model, active string }{
	"claude":   {PinProfile, PinModel, ""},
	"copilot":  {PinCopilotProfile, PinCopilotModel, PinCopilotActive},
	"codex":    {PinCodexProfile, PinCodexModel, PinCodexActive},
	"gemini":   {PinGeminiProfile, PinGeminiModel, PinGeminiActive},
	"opencode": {PinOpencodeProfile, "", PinOpencodeActive},
}

// AgentProfilePin is the <AGENT>_PROFILE pin key for agent ("" for an unknown agent).
func AgentProfilePin(agent string) string { return agentPinKeys[agent].profile }

// AgentActivePin is the <AGENT>_ACTIVE pin key for agent ("" for claude or an unknown agent).
func AgentActivePin(agent string) string { return agentPinKeys[agent].active }

// ValidAgentActive rejects any value but on|off, and off for claude.
func ValidAgentActive(agent, v string) error {
	if _, ok := agentPinKeys[agent]; !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	switch v {
	case AgentOn:
		return nil
	case AgentOff:
		if agent == "claude" {
			return fmt.Errorf("claude is always on")
		}
		return nil
	}
	return fmt.Errorf("active must be %q or %q, got %q", AgentOn, AgentOff, v)
}

// AgentActive reports whether agent is on (unset = on; claude is always on).
func AgentActive(agent string) bool {
	k := agentPinKeys[agent].active
	return k == "" || Pin(k) != AgentOff
}

// AgentPins reads the vault pins once and returns every agent's spec; claude's profile is the session's, else its pin.
func AgentPins() map[string]AgentPinSpec {
	pin, _ := pinLookup()
	out := make(map[string]AgentPinSpec, len(agentPinKeys))
	for agent, k := range agentPinKeys {
		spec := AgentPinSpec{Profile: pin(k.profile), Off: k.active != "" && pin(k.active) == AgentOff}
		if k.model != "" {
			spec.Model = pin(k.model)
		}
		if agent == "claude" {
			spec.Profile = cmp.Or(SessionProfile(), spec.Profile)
		}
		out[agent] = spec
	}
	return out
}

// ValidAgentPin checks v as agent's profile pin: a registry profile, or for opencode also a sub id.
func ValidAgentPin(reg *Registry, agent, v string) error {
	if _, ok := agentPinKeys[agent]; !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	if agent == "opencode" && slices.Contains(RouteOpencodeSubIDs, v) {
		return nil
	}
	if reg == nil || reg.Providers[v] == nil {
		return fmt.Errorf("%s: unknown profile %q", agent, strings.TrimSpace(v))
	}
	return nil
}
