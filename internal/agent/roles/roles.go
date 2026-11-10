// Package roles runs a task by role (explore, fleet, compare): a role names an agent, legs fan out under one limit.
package roles

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/vault"
)

// rolesKey is the vault Overlay entry holding the user's role table: a JSON array of Role.
const rolesKey = "dispatch-roles"

// Role maps a role name to the agent that runs it; Models is the default model set for compare.
type Role struct {
	Name        string            `json:"name"`
	Agent       string            `json:"agent"`
	Models      []string          `json:"models,omitempty"`
	Permissions agent.Permissions `json:"permissions,omitzero"` // the role layer, over the agent's defaults
}

// Seed is the built-in role table, replaced by the vault overlay when set.
var Seed = []Role{
	{Name: "explore", Agent: "copilot"}, // read-only enforced; gemini/opencode explore is refused (owner, 2026-11-09)
	{Name: "fleet", Agent: "copilot"},
	{Name: "compare", Agent: "opencode", Models: []string{"claude-sonnet-5-5", "gpt-6-1-sol", "gemini-3-8-flash", "kimi-k3"}},
}

// Lookup finds name in the vault overlay table if set and valid, else in Seed.
func Lookup(name string) (Role, error) {
	table := Seed
	if t, ok := vaultRoles(); ok {
		table = t
	}
	names := make([]string, 0, len(table))
	for _, r := range table {
		if r.Name == name {
			return r, nil
		}
		names = append(names, r.Name)
	}
	return Role{}, fmt.Errorf("unknown role %q (%s)", name, strings.Join(names, "|"))
}

func vaultRoles() ([]Role, bool) {
	var out []Role
	var found bool
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, rolesKey)
		if !ok || raw == "" {
			return nil
		}
		found = json.Unmarshal([]byte(raw), &out) == nil && len(out) > 0
		return nil
	})
	if err != nil || !found {
		return nil, false
	}
	return out, true
}

// SplitModels returns the non-empty, trimmed entries of a comma-separated --models value.
func SplitModels(s string) []string {
	var out []string
	for m := range strings.SplitSeq(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}
