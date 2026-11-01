package agentconfig

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// QualifyPinSpec is spec as "profile:model" when providerFlag names a profile other than the session's own.
func QualifyPinSpec(reg *providers.Registry, providerFlag, spec string) string {
	if providerFlag == "" {
		return spec
	}
	if name, model, ok := strings.Cut(spec, ":"); ok {
		if _, found := reg.Providers[name]; found && model != "" {
			return spec
		}
	}
	session := cmp.Or(providers.SessionProfile(), providers.AgentRecord("claude").Profile, reg.Default)
	if providerFlag == session {
		return spec
	}
	return providerFlag + ":" + spec
}

// ParseModelPins maps `alias=model` pins (plus unpin) to alias → spec: alias "" is every alias, spec "" unpins.
func ParseModelPins(reg *providers.Registry, profile string, unpin bool, pins []string) (map[string]string, error) {
	set := map[string]string{}
	if unpin {
		set[""] = ""
		for _, s := range models.AliasNames {
			set[s] = ""
		}
	}
	for _, p := range pins {
		alias, spec, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("--pin wants alias=model, got %q", p)
		}
		if alias == "all" {
			alias = ""
		} else if !slices.Contains(models.AliasNames, alias) {
			return nil, fmt.Errorf("unknown alias %q (opus|sonnet|haiku|fable|all)", alias)
		}
		set[alias] = QualifyPinSpec(reg, profile, spec)
	}
	return set, nil
}

// WithClaudeModels is r with the model pins in set applied: alias "" is every alias, "" spec clears.
func WithClaudeModels(r vault.AgentRecord, set map[string]string) vault.AgentRecord {
	r.Aliases = maps.Clone(r.Aliases)
	if r.Aliases == nil {
		r.Aliases = map[string]string{}
	}
	for alias, spec := range set {
		switch {
		case alias == "":
			r.Model = spec
		case spec == "":
			delete(r.Aliases, alias)
		default:
			r.Aliases[alias] = spec
		}
	}
	return r
}

// ApplyClaudeModels writes set onto claude's record in one vault update, then audits each pin.
func ApplyClaudeModels(set map[string]string) error {
	err := vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) { m["claude"] = WithClaudeModels(m["claude"], set) })
	if err != nil {
		return err
	}
	for _, alias := range slices.Sorted(maps.Keys(set)) {
		providers.AuditModelPin(alias, set[alias])
	}
	return nil
}
