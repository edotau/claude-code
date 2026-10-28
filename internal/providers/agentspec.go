package providers

import "slices"

// AgentSpec is one agent's row: the CLI, the wires it speaks, its router path per wire and its vault pin keys.
type AgentSpec struct {
	Name   string
	Binary string // executable on PATH; "" = claude, which the harness launches itself (not a roster agent)
	// Dialects are the wires the agent speaks, preferred first; none = it runs only on its own login.
	Dialects []string
	// RouterSuffix is the path appended to the router session base for a wire; it covers every Dialects entry.
	RouterSuffix map[string]string
	// ProfilePin, ModelPin and ActivePin are the agent's vault pin keys; ModelPin is "" when it has no model pin.
	ProfilePin, ModelPin, ActivePin string
}

// agentSpecs is the one table of per-agent facts, in roster order (claude first, then the third-party CLIs).
var agentSpecs = []AgentSpec{
	{Name: "claude", ProfilePin: PinProfile, ModelPin: PinModel},
	{
		Name: "codex", Binary: "codex", Dialects: []string{ProtocolResponses},
		RouterSuffix: map[string]string{ProtocolResponses: "/" + ProtocolResponses},
		ProfilePin:   PinCodexProfile, ModelPin: PinCodexModel, ActivePin: PinCodexActive,
	},
	{Name: "gemini", Binary: "agy", ProfilePin: PinGeminiProfile, ModelPin: PinGeminiModel, ActivePin: PinGeminiActive},
	{
		Name: "opencode", Binary: "opencode", Dialects: []string{ProtocolAnthropic, ProtocolGemini, ProtocolOpenAI},
		RouterSuffix: map[string]string{ProtocolAnthropic: "", ProtocolGemini: "/" + ProtocolGemini,
			ProtocolOpenAI: "/" + ProtocolOpenAI, ProtocolResponses: "/" + ProtocolResponses},
		ProfilePin: PinOpencodeProfile, ActivePin: PinOpencodeActive,
	},
	{
		Name: "copilot", Binary: "copilot", Dialects: []string{ProtocolAnthropic, ProtocolOpenAI},
		RouterSuffix: map[string]string{ProtocolAnthropic: "", ProtocolOpenAI: "/" + ProtocolOpenAI,
			ProtocolResponses: "/" + ProtocolResponses},
		ProfilePin: PinCopilotProfile, ModelPin: PinCopilotModel, ActivePin: PinCopilotActive,
	},
}

// AgentSpecs lists every agent row in roster order, claude included.
func AgentSpecs() []AgentSpec { return slices.Clone(agentSpecs) }

// LookupAgentSpec returns the row for name (claude included).
func LookupAgentSpec(name string) (AgentSpec, bool) {
	i := slices.IndexFunc(agentSpecs, func(s AgentSpec) bool { return s.Name == name })
	if i < 0 {
		return AgentSpec{}, false
	}
	return agentSpecs[i], true
}
