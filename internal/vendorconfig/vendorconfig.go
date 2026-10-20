// Package vendorconfig is the one spec for how each vendor CLI (codex, opencode, copilot, gemini)
// is permissioned, which MCP servers it sees, and what env a skill launch injects for it.
package vendorconfig

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

//go:embed defaults.json
var defaultsJSON []byte

// GuardRules are prompts carried over into a vendor opted in via Permission.UseGuardRules.
type GuardRules struct {
	Ask  []string `json:"ask"`
	Deny []string `json:"deny"`
}

// Permission is one vendor's approval policy: Mode is the common allow-all/flag-only switch;
// the rest are vendor-native knobs only that vendor's renderer consumes.
type Permission struct {
	Mode                  string `json:"mode"`
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

// Config is the merged spec: built-in defaults plus the user's env.d/vendors.json overlay.
type Config struct {
	GuardRules GuardRules        `json:"guardRules"`
	Vendors    map[string]Vendor `json:"vendors"`
}

// UserFile is the per-user overlay: a vendor entry replaces the built-in whole.
func UserFile() string { return filepath.Join(paths.EnvDir(), "vendors.json") }

// Load merges the embedded defaults with UserFile and returns defaults alone when it's absent.
func Load() (*Config, error) {
	user, err := os.ReadFile(UserFile())
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
	if len(overlay) > 0 {
		var o Config
		if err := json.Unmarshal(overlay, &o); err != nil {
			return nil, fmt.Errorf("%s: %w", UserFile(), err)
		}
		if len(o.GuardRules.Ask) > 0 || len(o.GuardRules.Deny) > 0 {
			cfg.GuardRules = o.GuardRules
		}
		if cfg.Vendors == nil {
			cfg.Vendors = map[string]Vendor{}
		}
		for name, raw := range rawVendors(overlay) {
			if raw == nil { // "name": null removes a built-in vendor
				delete(cfg.Vendors, name)
				continue
			}
			var v Vendor
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("%s: vendors.%s: %w", UserFile(), name, err)
			}
			cfg.Vendors[name] = v
		}
	}
	return &cfg, nil
}

// rawVendors re-decodes overlay's vendors map with json.RawMessage so a null entry survives
// unmarshal instead of being dropped (Vendor has no pointer semantics to carry removal through).
func rawVendors(overlay []byte) map[string]json.RawMessage {
	var doc struct {
		Vendors map[string]json.RawMessage `json:"vendors"`
	}
	if err := json.Unmarshal(overlay, &doc); err != nil {
		return nil
	}
	return doc.Vendors
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
