package router

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// ProviderMode is how a Claude provider takes part in an untyped dispatch's rotation.
type ProviderMode string

const (
	ModeRotate    ProviderMode = "rotate" // the default: an untyped dispatch rotates through it
	ModeExclusive ProviderMode = "exclusive"
)

// ProviderModes lists the modes in document order.
var ProviderModes = []string{string(ModeRotate), string(ModeExclusive)}

// ProviderModeError is an unknown provider mode; it never quotes the value.
type ProviderModeError struct{}

func (ProviderModeError) Error() string { return "want " + strings.Join(ProviderModes, ", ") }

// ParseProviderMode reads a mode; empty is the default (rotate), anything else unlisted is a ProviderModeError.
func ParseProviderMode(s string) (ProviderMode, error) {
	switch ProviderMode(s) {
	case "", ModeRotate:
		return ModeRotate, nil
	case ModeExclusive:
		return ModeExclusive, nil
	}
	return ModeRotate, ProviderModeError{}
}

// Role is who a dispatch acts as; the dispatch carries it, it is never read from prompt text.
type Role string

const (
	RoleOwner Role = "owner" // an owner or lead
	RoleFleet Role = "fleet" // the executing fleet
)

// Roles lists the roles in document order.
var Roles = []string{string(RoleOwner), string(RoleFleet)}

// RoleError is an unknown role; it never quotes the value.
type RoleError struct{}

func (RoleError) Error() string { return "want " + strings.Join(Roles, ", ") }

// ParseRole reads a role; the empty string and anything unlisted are a RoleError.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleOwner, RoleFleet:
		return Role(s), nil
	}
	return "", RoleError{}
}

// Models lists the Claude tiers a role may rotate through, strongest first.
func (r Role) Models() []string {
	switch r {
	case RoleOwner:
		return []string{models.Opus}
	case RoleFleet:
		return []string{models.Sonnet, models.Haiku}
	}
	return nil
}

// ClaudeProvider is one rotation provider and the Claude tiers it serves.
type ClaudeProvider struct {
	Name   string // anthropic, or <provider>-<profile> for a workspace row (databricks-dev)
	Models []string
}

// RoleCell is one (provider, model) pair a role may rotate through.
type RoleCell struct{ Provider, Model string }

// ClaudeProviders lists the registry's Claude-serving providers: a plain row by provider, a workspace by profile.
func ClaudeProviders(reg *providers.Registry) []ClaudeProvider {
	var out []ClaudeProvider
	for _, group := range providers.ProviderKeys() {
		for _, profile := range reg.ProfilesOf(group) {
			p := reg.Row(profile)
			name := group
			if p.Preset != "" {
				name = fmt.Sprintf("%s-%s", group, profile)
			}
			served := claudeTiers(p)
			if len(served) == 0 {
				continue
			}
			if i := slices.IndexFunc(out, func(c ClaudeProvider) bool { return c.Name == name }); i >= 0 {
				out[i].Models = mergeTiers(out[i].Models, served)
				continue
			}
			out = append(out, ClaudeProvider{Name: name, Models: served})
		}
	}
	return out
}

// claudeTiers lists the model aliases a row maps to a Claude id, in models.AliasNames order.
func claudeTiers(p *providers.Provider) []string {
	var out []string
	for _, alias := range models.AliasNames {
		if alias == models.Fable && p.Preset != "" { // a workspace never serves Fable
			continue
		}
		if id := p.Models[alias]; id != "" && models.FamilyKind(id) == models.FamilyClaude {
			out = append(out, alias)
		}
	}
	return out
}

func mergeTiers(a, b []string) []string {
	var out []string
	for _, alias := range models.AliasNames {
		if slices.Contains(a, alias) || slices.Contains(b, alias) {
			out = append(out, alias)
		}
	}
	return out
}

// RoleCells lists the cells a role may rotate through; Fable is in no role's tiers, so it is only named.
func RoleCells(role Role, provs []ClaudeProvider) []RoleCell {
	var out []RoleCell
	for _, p := range provs {
		for _, m := range role.Models() {
			if slices.Contains(p.Models, m) {
				out = append(out, RoleCell{Provider: p.Name, Model: m})
			}
		}
	}
	return out
}

// FableHint names the one provider that serves Fable when model is Fable; else "".
func FableHint(model string) string {
	fam, _, _, claude := models.ParseClaudeID(model)
	if !strings.EqualFold(model, models.Fable) && (!claude || fam != models.Fable) {
		return ""
	}
	return "Fable is served only on anthropic"
}
