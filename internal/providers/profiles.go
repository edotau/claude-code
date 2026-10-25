package providers

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Profiles is the per-agent account/model/effort table; Active names the set in force ("" = today's behavior).
type Profiles struct {
	Active string             `json:"active,omitempty"`
	Sets   map[string]Profile `json:"sets,omitempty"`
}

// Profile is one set: a row per agent.
type Profile map[string]AgentRow

// AgentRow is one agent's row in a profile.
type AgentRow struct {
	Account string            `json:"account,omitempty"` // a provider name in this registry
	Model   string            `json:"model,omitempty"`
	Effort  string            `json:"effort,omitempty"`
	Models  map[string]string `json:"models,omitempty"` // session only: slot → model id
}

// SlotMap is the row's slot → model map: Models, else the single Model as the opus slot, else nil.
func (r AgentRow) SlotMap() map[string]string {
	switch {
	case len(r.Models) > 0:
		return r.Models
	case r.Model != "":
		return map[string]string{"opus": r.Model}
	}
	return nil
}

// ProfileAgents are the valid profile row keys.
var ProfileAgents = []string{"session", "codex", "copilot", "opencode", "gemini", "api"}

// ActiveProfile returns the active set; ok is false when no profile is active or it names no set.
func (r *Registry) ActiveProfile() (name string, p Profile, ok bool) {
	if r.Profiles.Active == "" {
		return "", nil, false
	}
	p, ok = r.Profiles.Sets[r.Profiles.Active]
	if !ok {
		return "", nil, false
	}
	return r.Profiles.Active, p, true
}

// AgentRow returns the active set's row for agent; false when Active=="" or the set has no such row.
func (r *Registry) AgentRow(agent string) (AgentRow, bool) {
	_, p, ok := r.ActiveProfile()
	if !ok {
		return AgentRow{}, false
	}
	row, ok := p[agent]
	return row, ok
}

// validateProfiles checks the active name, row agent keys and row accounts.
func validateProfiles(r *Registry) []string {
	var errs []string
	if a := r.Profiles.Active; a != "" {
		if _, ok := r.Profiles.Sets[a]; !ok {
			errs = append(errs, fmt.Sprintf("profiles.active names unknown set %q", a))
		}
	}
	for _, s := range slices.Sorted(maps.Keys(r.Profiles.Sets)) {
		for agent, row := range r.Profiles.Sets[s] {
			if !slices.Contains(ProfileAgents, agent) {
				errs = append(errs, fmt.Sprintf("profiles.sets.%s: unknown agent %q (valid: %s)", s, agent,
					strings.Join(ProfileAgents, ", ")))
			}
			if _, ok := r.Providers[row.Account]; row.Account != "" && !ok {
				errs = append(errs, fmt.Sprintf("profiles.sets.%s.%s: account names unknown provider %q", s, agent, row.Account))
			}
		}
	}
	return errs
}

// AgentRequest is a caller's explicit input for one agent; "" means not given.
type AgentRequest struct{ Agent, Provider, Model, Effort string }

// AgentBinding is what an agent runs on after merging the request over the active profile's row.
type AgentBinding struct {
	Provider, Model, Effort string
	Slots                   map[string]string // session only: slot → model id
	Profile                 string            // profile that supplied anything; "" if none
}

// ResolveAgent merges explicit input over the active profile's row: flags > row > today's pins/defaults.
func (r *Registry) ResolveAgent(req AgentRequest) AgentBinding {
	b := AgentBinding{Provider: req.Provider, Model: req.Model, Effort: req.Effort}
	row, ok := r.AgentRow(req.Agent)
	if !ok {
		return b
	}
	fromRow := false
	providerFromRow := false
	if b.Provider == "" {
		prefix, _, hasPrefix := strings.Cut(req.Model, ":")
		if _, reg := r.Providers[prefix]; hasPrefix && reg && req.Agent != "session" { // session keeps the row's account
			b.Provider = prefix
		} else if row.Account != "" {
			b.Provider, providerFromRow, fromRow = row.Account, true, true
		}
	}
	if b.Model == "" && row.Model != "" && (providerFromRow || (b.Provider != "" && b.Provider == row.Account) || (row.Account == "" && req.Provider == "")) {
		b.Model, fromRow = row.Model, true
	}
	if b.Effort == "" && row.Effort != "" {
		b.Effort, fromRow = row.Effort, true
	}
	if providerFromRow && req.Agent == "session" {
		b.Slots = maps.Clone(row.SlotMap())
		if req.Model != "" {
			if b.Slots == nil {
				b.Slots = map[string]string{}
			}
			b.Slots["opus"] = req.Model
		}
	}
	if fromRow {
		b.Profile = r.Profiles.Active
	}
	return b
}
