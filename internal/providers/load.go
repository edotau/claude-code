package providers

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"

	"github.com/edotau/claude-code/internal/vault"
)

//go:embed defaults.json
var defaultsJSON []byte

// OverlayDoc returns vault overlay/providers, or fs.ErrNotExist when absent.
func OverlayDoc() ([]byte, error) {
	var doc []byte
	err := vault.View(func(tx *vault.Tx) error {
		raw, ok := tx.Get(vault.Overlay, "providers")
		if !ok {
			return fmt.Errorf("vault overlay/providers: %w", fs.ErrNotExist)
		}
		doc = []byte(raw)
		return nil
	})
	return doc, err
}

// Load merges the embedded defaults with the user overlay and validates the result.
func Load() (*Registry, error) {
	doc, err := OverlayDoc()
	if errors.Is(err, fs.ErrNotExist) {
		doc = nil
	} else if err != nil {
		return nil, err
	}
	return Parse(defaultsJSON, doc)
}

// Validate checks an overlay merged with the embedded defaults.
func Validate(overlay []byte) error {
	_, err := Parse(defaultsJSON, overlay)
	return err
}

// Parse merges base with an optional overlay document.
func Parse(base, overlay []byte) (*Registry, error) {
	var reg Registry
	if err := json.Unmarshal(base, &reg); err != nil {
		return nil, fmt.Errorf("providers defaults: %w", err)
	}
	if len(overlay) > 0 {
		var o Registry
		if err := json.Unmarshal(overlay, &o); err != nil {
			return nil, fmt.Errorf("vault overlay/providers: %w", err)
		}
		applyOverlay(&reg, &o)
	}
	for n, p := range reg.Providers {
		p.Name = n
	}
	return &reg, reg.Validate()
}

// applyOverlay merges overlay fields o into base, cell by cell for routes and whole-value for scalars.
func applyOverlay(base, o *Registry) {
	if o.Default != "" {
		base.Default = o.Default
	}
	if o.Fallback != nil {
		base.Fallback = o.Fallback
	}
	if o.OpencodeLadder != nil {
		base.OpencodeLadder = o.OpencodeLadder
	}
	if o.OpencodePlugins != nil {
		base.OpencodePlugins = o.OpencodePlugins
	}
	if o.AgentPlans != nil {
		base.AgentPlans = o.AgentPlans
	}
	if o.AgentPlanFormat != "" {
		base.AgentPlanFormat = o.AgentPlanFormat
	}
	for runner, tiers := range o.Route { // cell by cell, matching how routetable.LoadFrom applies it
		if base.Route == nil {
			base.Route = map[string]map[string]RouteCell{}
		}
		if base.Route[runner] == nil {
			base.Route[runner] = map[string]RouteCell{}
		}
		maps.Copy(base.Route[runner], tiers)
	}
	if o.RouteWeights != nil {
		base.RouteWeights = o.RouteWeights // an overlay replaces the weights whole, per its docstring
	}
	if base.Providers == nil {
		base.Providers = map[string]*Provider{}
	}
	for n, p := range o.Providers {
		if p == nil { // "name": null removes a built-in
			delete(base.Providers, n)
			continue
		}
		base.Providers[n] = p
	}
}
