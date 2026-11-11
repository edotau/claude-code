// Package agentconfig is each agent CLI's spec; this header indexes every agent's allow/deny policy source.
//
//   - Claude Code: internal/settings/template.json `permissions` (ask/deny), seeded into a missing settings.json.
//   - gemini: Claude's ask/deny carried over as GuardRules (templateGuardRules), opted in by
//     agents.json `useGuardRules`; converted in internal/gemini (agyGuardRules).
//   - codex, copilot, opencode launch knobs: agents.json (approvalPolicy, sandboxMode, launch args).
//   - safety-scan hook: internal/hooks/safety*.go blocks destructive shapes for every agent, on top of all of the above.
package agentconfig

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
)

//go:embed agents.json
var defaultsJSON []byte

// MCP is the explicit allowlist an agent's config is rendered with; omitted servers aren't wired.
type MCP struct {
	Allow []string `json:"allow"`
}

// LaunchConfig is env and argv a `claude-code <agent>` launch injects, e.g. to disable a native skill
// scanner or force allow-all; Args never rides a headless dispatch (tools-off must stay tools-off).
type LaunchConfig struct {
	Env  []string `json:"env,omitempty"`
	Args []string `json:"args,omitempty"`
}

// AgentConfig is one CLI's full spec.
type AgentConfig struct {
	Permission Permission   `json:"permission"`
	MCP        MCP          `json:"mcp"`
	Launch     LaunchConfig `json:"launch"`
}

// Config is the merged spec: built-in defaults plus the user's vault overlay/agents.
type Config struct {
	GuardRules GuardRules             `json:"guardRules"`
	Agents     map[string]AgentConfig `json:"agents"`
	// records are the built-in records with legacy overlay entries merged; Settings reads them.
	records map[string]vault.AgentRecord
}

// configOverlay is the user overlay (vault overlay/agents; an entry replaces the built-in whole); fs.ErrNotExist when absent.
func configOverlay() ([]byte, error) {
	var doc []byte
	found := false
	err := vault.View(func(t *vault.Tx) error {
		v, ok := t.Get(vault.Overlay, "agents")
		doc, found = []byte(v), ok
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("vault overlay/agents: %w", fs.ErrNotExist)
	}
	return doc, nil
}

// configCache holds the merged config per vault.db stamp; LoadConfig hands out clones.
var configCache paths.StampCache[*Config]

// LoadConfig merges the embedded defaults with configOverlay and returns defaults alone when it's absent.
func LoadConfig() (*Config, error) {
	cfg, err := configCache.Get(vault.Path(), loadConfig)
	return cfg.Clone(), err
}

// loadConfig is LoadConfig's uncached merge of the vault overlay over the embedded defaults.
func loadConfig() (*Config, error) {
	user, err := configOverlay()
	if errors.Is(err, fs.ErrNotExist) {
		user = nil
	} else if err != nil {
		return nil, err
	}
	return ParseConfig(defaultsJSON, user)
}

// Clone returns a deep copy of c; nil clones to nil.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := &Config{GuardRules: c.GuardRules.clone()}
	if c.Agents != nil {
		out.Agents = make(map[string]AgentConfig, len(c.Agents))
		for name, a := range c.Agents {
			out.Agents[name] = a.clone()
		}
	}
	if c.records != nil {
		out.records = make(map[string]vault.AgentRecord, len(c.records))
		for name, r := range c.records {
			out.records[name] = cloneRecord(r)
		}
	}
	return out
}

// cloneRecord returns a deep copy of r: aliases, extras and the policy and launch blocks.
func cloneRecord(r vault.AgentRecord) vault.AgentRecord {
	out := r
	out.Aliases, out.Extras = maps.Clone(r.Aliases), maps.Clone(r.Extras)
	if p := r.Permissions; p != nil {
		out.Permissions = new(vault.AgentPolicy(Policy(*p).clone()))
	}
	if l := r.Launch; l != nil {
		out.Launch = &vault.AgentLaunch{Env: slices.Clone(l.Env), Args: slices.Clone(l.Args)}
	}
	return out
}

// clone returns a deep copy of g's prompt lists.
func (g GuardRules) clone() GuardRules {
	return GuardRules{Ask: slices.Clone(g.Ask), Deny: slices.Clone(g.Deny)}
}

// clone returns a deep copy of p's rule lists.
func (p Policy) clone() Policy {
	p.Allow, p.Ask, p.Deny = slices.Clone(p.Allow), slices.Clone(p.Ask), slices.Clone(p.Deny)
	return p
}

// clone returns a deep copy of a: permission defaults, MCP allowlist and launch env/args.
func (a AgentConfig) clone() AgentConfig {
	out := a
	out.Permission.Defaults = a.Permission.Defaults.clone()
	out.MCP.Allow = slices.Clone(a.MCP.Allow)
	out.Launch.Env = slices.Clone(a.Launch.Env)
	out.Launch.Args = slices.Clone(a.Launch.Args)
	return out
}

// ParseConfig merges base with an optional overlay document. A nil base uses the embedded defaults.
func ParseConfig(base, overlay []byte) (*Config, error) {
	if base == nil {
		base = defaultsJSON
	}
	var defaults struct {
		Agents map[string]vault.AgentRecord `json:"agents"`
	}
	if err := json.Unmarshal(base, &defaults); err != nil {
		return nil, fmt.Errorf("agentconfig defaults: %w", err)
	}
	cfg := Config{records: defaults.Agents}
	if defaults.Agents != nil {
		cfg.Agents = make(map[string]AgentConfig, len(defaults.Agents))
	}
	for name, r := range defaults.Agents {
		cfg.Agents[name] = agentConfig(r)
	}
	// Built-in guard rules come from the settings template, not a hand-copied subset here.
	guard, err := templateGuardRules()
	if err != nil {
		return nil, err
	}
	cfg.GuardRules = guard
	if len(overlay) > 0 {
		if err := applyOverlay(&cfg, overlay); err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}

// applyOverlay merges the vault overlay document into cfg: guard rules replace, agents add or remove.
func applyOverlay(cfg *Config, overlay []byte) error {
	// Agents as json.RawMessage so a "name": null entry survives decode to signal removal.
	var o struct {
		GuardRules *GuardRules                `json:"guardRules"`
		Agents     map[string]json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(overlay, &o); err != nil {
		return fmt.Errorf("vault overlay/agents: %w", err)
	}
	if o.GuardRules != nil { // presence replaces wholesale, even an empty {"ask":[],"deny":[]}
		cfg.GuardRules = *o.GuardRules
	}
	if cfg.Agents == nil {
		cfg.Agents = map[string]AgentConfig{}
	}
	if cfg.records == nil {
		cfg.records = map[string]vault.AgentRecord{}
	}
	for name, raw := range o.Agents {
		if raw == nil || string(raw) == "null" { // "name": null removes a built-in agent
			delete(cfg.Agents, name)
			cfg.records[name] = mergeRecord(cfg.records[name], legacyRecord(AgentConfig{}))
			continue
		}
		var v AgentConfig
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("vault overlay/agents: agents.%s: %w", name, err)
		}
		cfg.Agents[name] = v
		cfg.records[name] = mergeRecord(cfg.records[name], legacyRecord(v))
	}
	return nil
}

// legacyRecord is a whole-replace overlay entry as a record: both blocks set and every knob key, "" when unset.
func legacyRecord(a AgentConfig) vault.AgentRecord {
	p, guard := a.Permission, ""
	if p.UseGuardRules {
		guard = "true"
	}
	return vault.AgentRecord{
		Permissions: new(vault.AgentPolicy(p.Defaults)),
		Launch:      new(vault.AgentLaunch(a.Launch)),
		Extras: map[string]string{extraApprovalPolicy: p.ApprovalPolicy, extraSandboxMode: p.SandboxMode,
			extraDefaultPermissionMode: p.DefaultPermissionMode, extraUseGuardRules: guard},
	}
}

// agentConfig is a built-in record in the legacy shape cfg.Agent returns until the old types go.
func agentConfig(r vault.AgentRecord) AgentConfig {
	var a AgentConfig
	if p := r.Permissions; p != nil {
		a.Permission.Defaults = Policy(*p)
	}
	if l := r.Launch; l != nil {
		a.Launch = LaunchConfig(*l)
	}
	a.Permission.ApprovalPolicy = r.Extras[extraApprovalPolicy]
	a.Permission.SandboxMode = r.Extras[extraSandboxMode]
	a.Permission.DefaultPermissionMode = r.Extras[extraDefaultPermissionMode]
	a.Permission.UseGuardRules = r.Extras[extraUseGuardRules] == "true"
	return a
}

// mergeRecord lays over on base: scalars and aliases when set, policy and launch whole when non-nil, extras by key.
func mergeRecord(base, over vault.AgentRecord) vault.AgentRecord {
	out := base
	out.Profile, out.Model = cmp.Or(over.Profile, base.Profile), cmp.Or(over.Model, base.Model)
	out.Effort, out.Context = cmp.Or(over.Effort, base.Effort), cmp.Or(over.Context, base.Context)
	out.Account, out.Fallback = cmp.Or(over.Account, base.Account), cmp.Or(over.Fallback, base.Fallback)
	if len(over.Aliases) > 0 {
		out.Aliases = over.Aliases
	}
	if over.Permissions != nil {
		out.Permissions = over.Permissions
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

// Settings is name's merged record: built-in defaults, then the legacy overlay entry, then the stored vault record.
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

// BuiltinSettings is Settings without the stored vault record: built-in defaults, then the legacy overlay entry.
func BuiltinSettings(name string) (vault.AgentRecord, error) {
	cfg, err := configCache.Get(vault.Path(), loadConfig)
	if err != nil {
		return vault.AgentRecord{}, err
	}
	return cloneRecord(cfg.records[name]), nil
}

// templateGuardRules reads the embedded settings template's permissions.ask/deny verbatim.
func templateGuardRules() (GuardRules, error) {
	var tpl struct {
		Permissions struct {
			Ask  []string `json:"ask"`
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(settings.Template(), &tpl); err != nil {
		return GuardRules{}, fmt.Errorf("agentconfig guard rules: %w", err)
	}
	return GuardRules{Ask: tpl.Permissions.Ask, Deny: tpl.Permissions.Deny}, nil
}

// Agent returns name's spec, or the zero value when Config is nil or name is unknown.
func (c *Config) Agent(name string) AgentConfig {
	if c == nil {
		return AgentConfig{}
	}
	return c.Agents[name]
}

// Guard returns the guard-rule prompts, or the zero value when Config is nil.
func (c *Config) Guard() GuardRules {
	if c == nil {
		return GuardRules{}
	}
	return c.GuardRules
}

// GuardRules are prompts carried over into an agent opted in via Permission.UseGuardRules.
type GuardRules struct {
	Ask  []string `json:"ask"`
	Deny []string `json:"deny"`
}

// Permission is one agent's approval policy: agent-native knobs only that agent's renderer consumes.
type Permission struct {
	UseGuardRules         bool   `json:"useGuardRules,omitempty"`
	ApprovalPolicy        string `json:"approvalPolicy,omitempty"`
	SandboxMode           string `json:"sandboxMode,omitempty"`
	DefaultPermissionMode string `json:"defaultPermissionMode,omitempty"`
	Defaults              Policy `json:"defaults,omitzero"` // the agent's default dispatch policy (agent.DefaultPermissions)
}

// Extras keys the legacy Permission knobs map to in a vault.AgentRecord.
const (
	extraApprovalPolicy        = "approvalPolicy"
	extraSandboxMode           = "sandboxMode"
	extraDefaultPermissionMode = "defaultPermissionMode"
	extraUseGuardRules         = "useGuardRules"
)

// Policy is a dispatch policy in Claude Code's rule grammar, e.g. deny ["Bash(rm:*)"]; mode default, auto or plan.
type Policy struct {
	Mode  string   `json:"mode,omitempty"`
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}
