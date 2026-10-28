// Package vendorconfig is the one spec for how each vendor CLI (codex, opencode, copilot, gemini)
// is permissioned, which MCP servers it sees, and what env a skill launch injects for it.
package vendorconfig

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
)

//go:embed defaults.json
var defaultsJSON []byte

// GuardRules are prompts carried over into a vendor opted in via Permission.UseGuardRules.
type GuardRules struct {
	Ask  []string `json:"ask"`
	Deny []string `json:"deny"`
}

// Permission is one vendor's approval policy: vendor-native knobs only that vendor's renderer consumes.
type Permission struct {
	UseGuardRules         bool   `json:"useGuardRules,omitempty"`
	ApprovalPolicy        string `json:"approvalPolicy,omitempty"`
	SandboxMode           string `json:"sandboxMode,omitempty"`
	DefaultPermissionMode string `json:"defaultPermissionMode,omitempty"`
}

// MCP is the explicit allowlist a vendor's config is rendered with; omitted servers aren't wired.
type MCP struct {
	Allow []string `json:"allow"`
}

// Launch is env and argv a `claude-code <vendor>` launch injects, e.g. to disable a native skill
// scanner or force allow-all; Args never rides a headless leg (tools-off must stay tools-off).
type Launch struct {
	Env  []string `json:"env,omitempty"`
	Args []string `json:"args,omitempty"`
}

// Vendor is one CLI's full spec.
type Vendor struct {
	Permission Permission `json:"permission"`
	MCP        MCP        `json:"mcp"`
	Launch     Launch     `json:"launch"`
}

// Config is the merged spec: built-in defaults plus the user's vault overlay/agents.
type Config struct {
	GuardRules GuardRules        `json:"guardRules"`
	Vendors    map[string]Vendor `json:"agents"`
}

// OverlayDoc is the user overlay (vault overlay/agents; an entry replaces the built-in whole); fs.ErrNotExist when absent.
func OverlayDoc() ([]byte, error) {
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

// Load merges the embedded defaults with OverlayDoc and returns defaults alone when it's absent.
func Load() (*Config, error) {
	user, err := OverlayDoc()
	if errors.Is(err, fs.ErrNotExist) {
		user = nil
	} else if err != nil {
		return nil, err
	}
	return Parse(defaultsJSON, user)
}

// Parse merges base with an optional overlay document. A nil base uses the embedded defaults.
func Parse(base, overlay []byte) (*Config, error) {
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
		// Vendors as json.RawMessage so a "name": null entry survives decode to signal removal.
		var o struct {
			GuardRules *GuardRules                `json:"guardRules"`
			Vendors    map[string]json.RawMessage `json:"agents"`
		}
		if err := json.Unmarshal(overlay, &o); err != nil {
			return nil, fmt.Errorf("vault overlay/agents: %w", err)
		}
		if o.GuardRules != nil { // presence replaces wholesale, even an empty {"ask":[],"deny":[]}
			cfg.GuardRules = *o.GuardRules
		}
		if cfg.Vendors == nil {
			cfg.Vendors = map[string]Vendor{}
		}
		for name, raw := range o.Vendors {
			if raw == nil || string(raw) == "null" { // "name": null removes a built-in vendor
				delete(cfg.Vendors, name)
				continue
			}
			var v Vendor
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("vault overlay/agents: agents.%s: %w", name, err)
			}
			cfg.Vendors[name] = v
		}
	}
	return &cfg, nil
}

// templateGuardRules reads the embedded settings template's permissions.ask/deny verbatim.
func templateGuardRules() (GuardRules, error) {
	rendered, err := settings.Render()
	if err != nil {
		return GuardRules{}, fmt.Errorf("vendorconfig guard rules: %w", err)
	}
	var tpl struct {
		Permissions struct {
			Ask  []string `json:"ask"`
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(rendered, &tpl); err != nil {
		return GuardRules{}, fmt.Errorf("vendorconfig guard rules: %w", err)
	}
	return GuardRules{Ask: tpl.Permissions.Ask, Deny: tpl.Permissions.Deny}, nil
}

// Vendor returns name's spec, or the zero value when Config is nil or name is unknown.
func (c *Config) Vendor(name string) Vendor {
	if c == nil {
		return Vendor{}
	}
	return c.Vendors[name]
}

// Guard returns the guard-rule prompts, or the zero value when Config is nil.
func (c *Config) Guard() GuardRules {
	if c == nil {
		return GuardRules{}
	}
	return c.GuardRules
}
