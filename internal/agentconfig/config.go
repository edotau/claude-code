// Package agentconfig is each agent CLI's spec; this header indexes every agent's allow/deny policy source.
//
//   - Claude Code: internal/settings/template.json `permissions` (ask/deny), seeded into a missing settings.json.
//   - gemini: Claude's deny list carried over as GuardRules (templateGuardRules), on by default unless the
//     agent's `useGuardRules` extra is "false"; converted in internal/gemini (agyGuardRules).
//   - codex, copilot, opencode launch knobs: agents.json (approvalPolicy, sandboxMode, launch args).
//   - safety-scan hook: internal/hooks/safety*.go blocks destructive shapes for every agent, on top of all of the above.
package agentconfig

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strconv"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
)

//go:embed agents.json
var defaultsJSON []byte

// agentConfig is one CLI's full spec.
type agentConfig struct {
	Permission permission        `json:"permission"`
	Launch     vault.AgentLaunch `json:"launch"`
}

// Config is the merged spec: built-in defaults plus the user's vault overlay/agents.
type Config struct {
	GuardRules guardRules `json:"guardRules"`
	// records are the built-in records with legacy overlay entries merged; Settings reads them.
	records map[string]vault.AgentRecord
}

// configOverlay is the vault overlay/agents document (an entry replaces the built-in whole); fs.ErrNotExist if absent.
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
	return ParseConfig(user)
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
		c := *p
		c.Allow, c.Ask, c.Deny = slices.Clone(p.Allow), slices.Clone(p.Ask), slices.Clone(p.Deny)
		out.Permissions = &c
	}
	if l := r.Launch; l != nil {
		out.Launch = &vault.AgentLaunch{Env: slices.Clone(l.Env), Args: slices.Clone(l.Args)}
	}
	return out
}

// ParseConfig merges the embedded defaults with an optional overlay document.
func ParseConfig(overlay []byte) (*Config, error) {
	var defaults struct {
		Agents map[string]vault.AgentRecord `json:"agents"`
	}
	if err := json.Unmarshal(defaultsJSON, &defaults); err != nil {
		return nil, fmt.Errorf("agentconfig defaults: %w", err)
	}
	cfg := Config{records: defaults.Agents}
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
	// Agents as json.RawMessage so a "name": null entry survives decode to signal clearing.
	var o struct {
		Comment    json.RawMessage            `json:"_comment"` // free-text note; accepted and ignored
		GuardRules *guardRules                `json:"guardRules"`
		Agents     map[string]json.RawMessage `json:"agents"`
	}
	if err := decodeStrict(overlay, &o); err != nil {
		return fmt.Errorf("vault overlay/agents: %w", err)
	}
	if o.GuardRules != nil { // presence replaces wholesale, even an empty {"deny":[]}
		cfg.GuardRules = *o.GuardRules
	}
	if cfg.records == nil {
		cfg.records = map[string]vault.AgentRecord{}
	}
	for name, raw := range o.Agents {
		if raw == nil || string(raw) == "null" { // "name": null clears a built-in agent's knobs
			cfg.records[name] = mergeRecord(cfg.records[name], legacyRecord(agentConfig{}))
			continue
		}
		var v agentConfig
		if err := decodeStrict(raw, &v); err != nil {
			return fmt.Errorf("vault overlay/agents: agents.%s: %w", name, err)
		}
		cfg.records[name] = mergeRecord(cfg.records[name], legacyRecord(v))
	}
	return nil
}

// decodeStrict decodes one JSON value into v, refusing unknown keys and trailing data.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// legacyRecord is a whole-replace overlay entry as a record: both blocks set and every knob key, "" when unset.
func legacyRecord(a agentConfig) vault.AgentRecord {
	p, guard := a.Permission, ""
	if p.UseGuardRules != nil {
		guard = strconv.FormatBool(*p.UseGuardRules) // an explicit false must survive mergeExtras, which drops ""
	}
	return vault.AgentRecord{
		Permissions: new(p.Defaults),
		Launch:      new(a.Launch),
		Extras: map[string]string{extraApprovalPolicy: p.ApprovalPolicy, extraSandboxMode: p.SandboxMode,
			extraDefaultPermissionMode: p.DefaultPermissionMode, extraUseGuardRules: guard},
	}
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

// Guard returns the guard rules, or the zero value when Config is nil.
func (c *Config) Guard() guardRules {
	if c == nil {
		return guardRules{}
	}
	return c.GuardRules
}

// guardRules are the deny rules carried over into agents that honour Permission.UseGuardRules.
type guardRules struct {
	Deny []string `json:"deny"`
}

// permission is one agent's approval policy: agent-native knobs only that agent's renderer consumes.
type permission struct {
	UseGuardRules         *bool             `json:"useGuardRules,omitempty"`
	ApprovalPolicy        string            `json:"approvalPolicy,omitempty"`
	SandboxMode           string            `json:"sandboxMode,omitempty"`
	DefaultPermissionMode string            `json:"defaultPermissionMode,omitempty"`
	Defaults              vault.AgentPolicy `json:"defaults,omitzero"` // the agent's default dispatch policy (agent.DefaultPermissions)
}

// Extras keys the legacy Permission knobs map to in a vault.AgentRecord.
const (
	extraApprovalPolicy        = "approvalPolicy"
	extraSandboxMode           = "sandboxMode"
	extraDefaultPermissionMode = "defaultPermissionMode"
	extraUseGuardRules         = "useGuardRules"
)
