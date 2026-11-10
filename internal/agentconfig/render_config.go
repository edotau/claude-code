package agentconfig

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
}

// ConfigOverlay is the user overlay (vault overlay/agents; an entry replaces the built-in whole); fs.ErrNotExist when absent.
func ConfigOverlay() ([]byte, error) {
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

// LoadConfig merges the embedded defaults with ConfigOverlay and returns defaults alone when it's absent.
func LoadConfig() (*Config, error) {
	cfg, err := configCache.Get(vault.Path(), loadConfig)
	return cfg.Clone(), err
}

// loadConfig is LoadConfig's uncached merge of the vault overlay over the embedded defaults.
func loadConfig() (*Config, error) {
	user, err := ConfigOverlay()
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
	return out
}

// clone returns a deep copy of g's prompt lists.
func (g GuardRules) clone() GuardRules {
	return GuardRules{Ask: slices.Clone(g.Ask), Deny: slices.Clone(g.Deny)}
}

// clone returns a deep copy of a: permission defaults, MCP allowlist and launch env/args.
func (a AgentConfig) clone() AgentConfig {
	out := a
	d := a.Permission.Defaults
	out.Permission.Defaults = Policy{Mode: d.Mode, Allow: slices.Clone(d.Allow), Ask: slices.Clone(d.Ask), Deny: slices.Clone(d.Deny)}
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
	var cfg Config
	if err := json.Unmarshal(base, &cfg); err != nil {
		return nil, fmt.Errorf("vendorconfig defaults: %w", err)
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
	for name, raw := range o.Agents {
		if raw == nil || string(raw) == "null" { // "name": null removes a built-in agent
			delete(cfg.Agents, name)
			continue
		}
		var v AgentConfig
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("vault overlay/agents: agents.%s: %w", name, err)
		}
		cfg.Agents[name] = v
	}
	return nil
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
		return GuardRules{}, fmt.Errorf("vendorconfig guard rules: %w", err)
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
