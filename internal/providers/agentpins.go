package providers

import (
	"cmp"
	"errors"
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

// AgentProfilePin is the <AGENT>_PROFILE pin key for agent ("" for an unknown agent).
func AgentProfilePin(agent string) string { s, _ := LookupAgentSpec(agent); return s.ProfilePin }

// AgentActivePin is the <AGENT>_ACTIVE pin key for agent ("" for claude or an unknown agent).
func AgentActivePin(agent string) string { s, _ := LookupAgentSpec(agent); return s.ActivePin }

// ValidAgentActive rejects any value but on|off, and off for claude.
func ValidAgentActive(agent, v string) error {
	if _, ok := LookupAgentSpec(agent); !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	switch v {
	case AgentOn:
		return nil
	case AgentOff:
		if agent == "claude" {
			return errors.New("claude is always on")
		}
		return nil
	}
	return fmt.Errorf("active must be %q or %q, got %q", AgentOn, AgentOff, v)
}

// AgentActive reports whether agent is on (unset = on; claude is always on).
func AgentActive(agent string) bool {
	k := AgentActivePin(agent)
	return k == "" || Pin(k) != AgentOff
}

// AgentPins reads the vault pins once and returns every agent's spec; claude's profile is the session's, else its pin.
func AgentPins() map[string]AgentPinSpec {
	pin, _ := pinLookup()
	out := make(map[string]AgentPinSpec, len(agentSpecs))
	for _, k := range agentSpecs {
		spec := AgentPinSpec{Profile: pin(k.ProfilePin), Off: k.ActivePin != "" && pin(k.ActivePin) == AgentOff}
		if k.ModelPin != "" {
			spec.Model = pin(k.ModelPin)
		}
		if k.Name == "claude" {
			spec.Profile = cmp.Or(SessionProfile(), spec.Profile)
		}
		out[k.Name] = spec
	}
	return out
}

// ValidAgentPin checks v as agent's profile pin: a registry profile, or for opencode also a sub id.
func ValidAgentPin(reg *Registry, agent, v string) error {
	if _, ok := LookupAgentSpec(agent); !ok {
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
