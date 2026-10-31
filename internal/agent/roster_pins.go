package agent

import (
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// claudeTiers are claude's tier pins, folded into its MODEL cell as tier=id.
var claudeTiers = []string{"OPUS", "SONNET", "HAIKU", "FABLE"}

// VaultAgentNames is claude, then the other AgentSpecs agents alphabetically, then extra overlay agent names.
func VaultAgentNames(vs map[string]any) []string {
	var names, rest []string
	for _, sp := range providers.AgentSpecs() {
		if sp.Binary == "" {
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
	return !VaultAgentIsClaude(name) && strings.TrimSpace(pins[strings.ToUpper(name)+"_ACTIVE"]) == providers.AgentOff
}
