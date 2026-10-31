package agent

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// claudeTiers are claude's tier pins, folded into its MODEL cell as tier=id.
var claudeTiers = []string{"OPUS", "SONNET", "HAIKU", "FABLE"}

// VaultAgentNames is claude, then the other roster agents alphabetically, then extra overlay agent names.
func VaultAgentNames(vs map[string]any) []string {
	var names, rest []string
	for _, sp := range Roster() {
		if !sp.External() {
			names = append(names, sp.Name)
		} else {
			rest = append(rest, sp.Name)
		}
	}
	names = append(names, slices.Sorted(slices.Values(rest))...)
	for _, name := range slices.Sorted(maps.Keys(vs)) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// VaultAgentPins is one agent's pin suffixes (after "<AGENT>_") to values; claude's tier pins fold into MODEL.
func VaultAgentPins(name string, pins map[string]string) map[string]string {
	prefix, out := strings.ToUpper(name)+"_", map[string]string{}
	for k, v := range pins {
		if suffix, ok := strings.CutPrefix(k, prefix); ok && strings.TrimSpace(v) != "" {
			out[suffix] = v
		}
	}
	if !VaultAgentIsClaude(name) {
		return out
	}
	var tiers []string
	for _, t := range claudeTiers {
		if v, ok := out[t+"_MODEL"]; ok {
			tiers = append(tiers, strings.ToLower(t)+"="+v)
		}
		delete(out, t+"_MODEL")
	}
	if len(tiers) > 0 {
		out["MODEL"] = strings.TrimSpace(out["MODEL"] + " " + strings.Join(tiers, " "))
	}
	return out
}

// VaultAgentIsClaude reports the roster row the harness launches itself (no external binary).
func VaultAgentIsClaude(name string) bool {
	s, ok := LookupSchema(name)
	return ok && !s.External()
}

// VaultAgentOff reports an agent whose ACTIVE pin says off (claude is always on).
func VaultAgentOff(name string, pins map[string]string) bool {
	return !VaultAgentIsClaude(name) && strings.TrimSpace(pins[strings.ToUpper(name)+"_ACTIVE"]) == AgentOff
}

// AgentOn and AgentOff are the <AGENT>_ACTIVE pin values; unset means on.
const (
	AgentOn  = "on"
	AgentOff = "off"
)

// AgentProfilePin is the <AGENT>_PROFILE pin key for agent ("" for an unknown agent).
func AgentProfilePin(agent string) string { s, _ := LookupSchema(agent); return s.CLI.ProfilePin }

// AgentActivePin is the <AGENT>_ACTIVE pin key for agent ("" for claude or an unknown agent).
func AgentActivePin(agent string) string { s, _ := LookupSchema(agent); return s.CLI.ActivePin }

// ValidAgentActive rejects any value but on|off, and off for claude.
func ValidAgentActive(agent, v string) error {
	if _, ok := LookupSchema(agent); !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	switch v {
	case AgentOn:
		return nil
	case AgentOff:
		if AgentActivePin(agent) == "" {
			return errors.New("claude is always on")
		}
		return nil
	}
	return fmt.Errorf("active must be %q or %q, got %q", AgentOn, AgentOff, v)
}

// AgentUnbillable is why copilot has no account to bill (no GitHub login under 95%, Databricks profile or key); "" else.
func AgentUnbillable(agent string) string {
	if agent != "copilot" {
		return ""
	}
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	return copilotUnbillable(reg)
}

func copilotUnbillable(reg *providers.Registry) string {
	if describeCopilotRoute(reg).OK {
		return ""
	}
	return "no account to bill"
}

// AgentActive reports whether agent is on (unset = on; claude is always on).
func AgentActive(agent string) bool {
	k := AgentActivePin(agent)
	return k == "" || providers.Pin(k) != AgentOff
}

// AgentPins reads the vault pins once and returns every agent's spec; claude's profile is the session's, else its pin.
func AgentPins() map[string]router.PinSpec {
	pin, _ := providers.PinLookup()
	out := make(map[string]router.PinSpec, len(rosterRows))
	for _, k := range rosterRows {
		spec := router.PinSpec{Profile: pin(k.CLI.ProfilePin), Off: k.CLI.ActivePin != "" && pin(k.CLI.ActivePin) == AgentOff}
		if k.CLI.ModelPin != "" {
			spec.Model = pin(k.CLI.ModelPin)
		}
		if !k.External() {
			spec.Profile = cmp.Or(providers.SessionProfile(), spec.Profile)
		}
		out[k.Name] = spec
	}
	return out
}

// ValidAgentPin checks v as agent's profile pin: a registry profile, or for opencode also a sub id.
func ValidAgentPin(reg *providers.Registry, agent, v string) error {
	if _, ok := LookupSchema(agent); !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	if agent == "opencode" && slices.Contains(providers.RouteOpencodeSubIDs, v) {
		return nil
	}
	if reg == nil || reg.Providers[v] == nil {
		return fmt.Errorf("%s: unknown profile %q", agent, strings.TrimSpace(v))
	}
	return nil
}
