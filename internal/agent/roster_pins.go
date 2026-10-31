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

// VaultAgentIsClaude reports the roster row the harness launches itself (no external binary).
func VaultAgentIsClaude(name string) bool {
	s, ok := LookupSchema(name)
	return ok && !s.External()
}

// AgentOn and AgentOff are the values `<agent> on|off` and ValidAgentActive accept.
const (
	AgentOn  = "on"
	AgentOff = "off"
)

// ValidAgentActive rejects any value but on|off, and off for claude.
func ValidAgentActive(agent, v string) error {
	if _, ok := LookupSchema(agent); !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	switch v {
	case AgentOn:
		return nil
	case AgentOff:
		if VaultAgentIsClaude(agent) {
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

// AgentActive reports whether agent is on: claude always, else a profile is set (copilot's no-bill-off stays derived).
func AgentActive(agent string) bool {
	if agent == "claude" {
		return true
	}
	if _, ok := LookupSchema(agent); !ok {
		return true
	}
	return GetAgentRecord(agent).Profile != ""
}

// AgentPins reads the records once and returns every agent's spec; claude's profile is the session's, else its record's.
func AgentPins() map[string]router.PinSpec {
	recs := AgentRecords()
	reg, _ := providers.Load()
	out := make(map[string]router.PinSpec, len(rosterRows))
	for _, k := range rosterRows {
		r := recs[k.Name]
		spec := router.PinSpec{Profile: r.Profile, Off: k.External() && r.Profile == ""}
		if ownLogin(reg, k.Name, r.Profile) {
			spec.Profile = "" // the agent's own login profile routes as the old unset pin did
		}
		spec.Model = r.Model
		if !k.External() {
			spec.Profile = cmp.Or(providers.SessionProfile(), spec.Profile)
		}
		out[k.Name] = spec
	}
	return out
}

// OpencodeOwnLogin is opencode's own-login profile: the free Zen catalog then the paid ladder, as the old unset pin.
const OpencodeOwnLogin = "zen-free"

// ownLogin reports a profile that is agent's own sign-in: a registry row with Auth.Login == agent, or opencode's Zen id.
func ownLogin(reg *providers.Registry, agent, profile string) bool {
	if profile == "" {
		return false
	}
	if agent == "opencode" {
		return profile == OpencodeOwnLogin
	}
	if reg == nil {
		return false
	}
	p := reg.Providers[profile]
	return p != nil && p.Auth.Login == agent
}

// ValidAgentPin checks v as agent's profile pin: a registry profile, or for opencode also a sub id.
func ValidAgentPin(reg *providers.Registry, agent, v string) error {
	if _, ok := LookupSchema(agent); !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	if agent == "opencode" && (v == OpencodeOwnLogin || slices.Contains(providers.RouteOpencodeSubIDs, v)) {
		return nil
	}
	if reg == nil || reg.Providers[v] == nil {
		return fmt.Errorf("%s: unknown profile %q", agent, strings.TrimSpace(v))
	}
	return nil
}
