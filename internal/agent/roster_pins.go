package agent

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
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

// agentModelEnv is each CLI agent's own model knob; claude's ANTHROPIC_MODEL is handled in Claude().
var agentModelEnv = map[string]string{
	"codex": "CODEX_MODEL", "copilot": "COPILOT_MODEL", "gemini": "GEMINI_MODEL", "opencode": "OPENCODE_MODEL",
}

// AgentModelPin is the agent's own model: its shell model env (local config) over the vault record's model, or "".
func AgentModelPin(agent string) string {
	if v := strings.TrimSpace(os.Getenv(agentModelEnv[agent])); v != "" && agentModelEnv[agent] != "" {
		return v
	}
	return vault.GetAgentRecord(agent).Model
}

// ResolveAgentModel is the one rule for the model an agent runs: flag > the agent's shell model env > its vault pin > Target.Model,
// spelled for Target.Provider. A model alias the provider lacks keeps Target.Model and says so in the note.
func ResolveAgentModel(a ModelAsk) (providers.Target, string) {
	t, p := a.Target, a.Target.Provider
	if a.Flag == "" {
		return pinnedAgentModel(a.Agent, t), ""
	}
	if a.Applied {
		return t, ""
	}
	alias := slices.Contains(models.AliasNames, a.Flag)
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

func pinnedAgentModel(agent string, t providers.Target) providers.Target {
	if pin := AgentModelPin(agent); pin != "" {
		if m := providers.SpellPin(t.Provider, pin); m != "" {
			t.Model = m
		}
	}
	return t
}

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
		return errUnknownAgent(agent)
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
	if SchemaFor("copilot").Eng().DescribeRoute(reg).OK {
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
	return vault.GetAgentRecord(agent).Profile != ""
}

// AgentPins reads the records once and returns every agent's spec; claude's profile is the session's, else its record's.
func AgentPins() (map[string]router.PinSpec, error) {
	recs, err := vault.ReadAgentRecords(RosterNames())
	if err != nil {
		return nil, fmt.Errorf("agent pins: %w", err)
	}
	reg := sync.OnceValue(func() *providers.Registry { r, _ := providers.Load(); return r }) // only a pinned row reads it
	out := make(map[string]router.PinSpec, len(rows()))
	for _, k := range rows() {
		r := recs[k.Name]
		spec := router.PinSpec{Profile: r.Profile, Off: k.External() && r.Profile == ""}
		if ownLogin(reg, k.Name, r.Profile) {
			spec.Profile = "" // the agent's own login profile routes as the old unset pin did
		}
		spec.Model = r.Model
		if !k.External() {
			spec.Profile = cmp.Or(providers.SessionLead(nil, false), spec.Profile)
		}
		out[k.Name] = spec
	}
	return out, nil
}

// ownLogin reports a profile that is agent's own sign-in: its schema's OwnLoginProfile, else a row with Auth.Login == agent.
func ownLogin(loadReg func() *providers.Registry, agent, profile string) bool {
	if profile == "" {
		return false
	}
	if s, ok := LookupSchema(agent); ok && s.CLI.OwnLoginProfile != "" {
		return profile == s.CLI.OwnLoginProfile
	}
	reg := loadReg()
	if reg == nil {
		return false
	}
	p := reg.Providers[profile]
	return p != nil && p.Auth.Login == agent
}

// ValidAgentPin checks v as agent's profile pin: a registry profile, or its schema's own-login id or extra pin ids.
func ValidAgentPin(reg *providers.Registry, agent, v string) error {
	s, ok := LookupSchema(agent)
	if !ok {
		return errUnknownAgent(agent)
	}
	if v != "" && v == s.CLI.OwnLoginProfile || slices.Contains(s.CLI.PinIDs, v) {
		return nil
	}
	if reg == nil || reg.Providers[v] == nil {
		return fmt.Errorf("%s: unknown profile %q", agent, strings.TrimSpace(v))
	}
	return nil
}
