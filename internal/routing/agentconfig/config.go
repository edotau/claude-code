// Package agentconfig is each agent CLI's spec; this header indexes every agent's allow/deny policy source.
//
//   - Claude Code: internal/assets/settings/template.json `permissions` (ask/deny), seeded into a missing settings.json.
//   - gemini: Claude's live deny list (template fallback) carried over as GuardRules, on by default unless the
//     agent's `useGuardRules` extra is "false"; converted in internal/gemini (agyGuardRules).
//   - codex, copilot, opencode launch knobs: internal/assets/agentconfig/agents.json (approvalPolicy, sandboxMode, launch args).
//   - safety-scan hook: internal/hooks/safety/ guards destructive shapes for every agent, on top of all of the above.
package agentconfig

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// Config is the built-in spec: agents.json records plus the settings template's guard rules.
type Config struct {
	GuardRules guardRules `json:"guardRules"`
	records    map[string]vault.AgentRecord
}

// loadConfigOnce parses the embedded defaults once; LoadConfig hands out clones.
var loadConfigOnce = sync.OnceValues(parseConfig)

// LoadConfig returns the embedded defaults; a leftover legacy overlay/agents key is ignored.
func LoadConfig() (*Config, error) {
	cfg, err := loadConfigOnce()
	return cfg.Clone(), err
}

// Clone returns a deep copy of c; nil clones to nil.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := &Config{GuardRules: guardRules{Deny: slices.Clone(c.GuardRules.Deny)}}
	if c.records != nil {
		out.records = make(map[string]vault.AgentRecord, len(c.records))
		for name, r := range c.records {
			out.records[name] = r.Clone()
		}
	}
	return out
}

// parseConfig reads the embedded agent defaults and the settings template's guard rules.
func parseConfig() (*Config, error) {
	var defaults struct {
		Agents map[string]vault.AgentRecord `json:"agents"`
	}
	if err := json.Unmarshal(assets.AgentDefaults(), &defaults); err != nil {
		return nil, fmt.Errorf("agentconfig defaults: %w", err)
	}
	guard, err := templateGuardRules()
	if err != nil {
		return nil, err
	}
	return &Config{GuardRules: guard, records: defaults.Agents}, nil
}

// mergeRecord lays over on base: scalars and aliases when set, policy and launch whole when non-nil, extras by key.
func mergeRecord(base, over vault.AgentRecord) vault.AgentRecord {
	out := base
	out.Model, out.Effort = cmp.Or(over.Model, base.Model), cmp.Or(over.Effort, base.Effort)
	out.Context = cmp.Or(over.Context, base.Context)
	if !over.EffectiveRoute().IsZero() { // a route is taken whole from one layer
		out.Route = over.Route
	}
	if !over.EffectiveFallbackRoute().IsZero() {
		out.FallbackRoute = over.FallbackRoute
	}
	if len(over.Aliases) > 0 {
		out.Aliases = over.Aliases
	}
	if over.Launch != nil {
		out.Launch = over.Launch
	}
	out.Extras = mergeExtras(base.Extras, over.Extras)
	return out
}

// mergeExtras is base with over's keys set, a "" value deleting the key; nil when nothing is left.
func mergeExtras(base, over map[string]string) map[string]string {
	out := maps.Clone(base)
	for k, v := range over {
		if v == "" {
			delete(out, k)
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Settings is name's merged record: built-in defaults, then the stored vault record.
func Settings(name string) (vault.AgentRecord, error) {
	base, err := BuiltinSettings(name)
	if err != nil {
		return vault.AgentRecord{}, err
	}
	recs, err := vault.ReadAgentRecords([]string{name})
	if err != nil {
		return vault.AgentRecord{}, err
	}
	return mergeRecord(base, recs[name]), nil
}

// BuiltinSettings is Settings without the stored vault record: the agents.json defaults only.
func BuiltinSettings(name string) (vault.AgentRecord, error) {
	cfg, err := loadConfigOnce()
	if err != nil {
		return vault.AgentRecord{}, err
	}
	return cfg.records[name].Clone(), nil
}

// templateGuardRules reads the embedded settings template's permissions.deny verbatim.
func templateGuardRules() (guardRules, error) {
	var tpl struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(settings.Template(), &tpl); err != nil {
		return guardRules{}, fmt.Errorf("agentconfig guard rules: %w", err)
	}
	return guardRules{Deny: tpl.Permissions.Deny}, nil
}

// Guard returns the live denies, else the template defaults; a nil Config returns zero.
func (c *Config) Guard() guardRules {
	if c == nil {
		return guardRules{}
	}
	if live, err := settings.LivePermissions(); err == nil {
		return guardRules{Deny: live.Deny}
	}
	return c.GuardRules
}

// guardRules are the deny rules carried over into agents that honour Permission.UseGuardRules.
type guardRules struct {
	Deny []string `json:"deny"`
}

var idRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_-]+$`) })

// OwnedID namespaces the provider entry so it never overwrites a user's own block of the same name.
func OwnedID(p *providers.Provider) (string, error) {
	if !idRE().MatchString(p.Name) {
		return "", fmt.Errorf("provider name %q must match %s to be passed to an agent config", p.Name, idRE())
	}
	return "harness-" + p.Name, nil
}

// CatalogWarn receives catalog warnings (codex and opencode menus).
var CatalogWarn io.Writer = os.Stderr

// SetCatalogWarn redirects catalog warnings and returns the previous writer; tests use it to keep stderr quiet.
func SetCatalogWarn(w io.Writer) io.Writer { old := CatalogWarn; CatalogWarn = w; return old }

// MenuWire spells a bare menu id the way p serves it; an id outside the catalog still takes the row's prefix once.
func MenuWire(p *providers.Provider, id string) string {
	if w := p.WireModel(id); w != id {
		return w
	}
	rule := p.SpellingRule()
	if _, _, known := models.Resolve(id); known || rule == "" ||
		strings.HasPrefix(id, models.DatabricksPrefix) {
		return id
	}
	return models.Wire(rule, id)
}
