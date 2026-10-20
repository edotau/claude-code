package providers

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

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
		if o.Default != "" {
			reg.Default = o.Default
		}
		if o.Fallback != nil {
			reg.Fallback = o.Fallback
		}
		if o.OpencodeLadder != nil {
			reg.OpencodeLadder = o.OpencodeLadder
		}
		if o.OpencodePlugins != nil {
			reg.OpencodePlugins = o.OpencodePlugins
		}
		if o.AgentPlans != nil {
			reg.AgentPlans = o.AgentPlans
		}
		if o.AgentPlanFormat != "" {
			reg.AgentPlanFormat = o.AgentPlanFormat
		}
		if reg.Providers == nil {
			reg.Providers = map[string]*Provider{}
		}
		for n, p := range o.Providers {
			if p == nil { // "name": null removes a built-in
				delete(reg.Providers, n)
				continue
			}
			reg.Providers[n] = p
		}
	}
	for n, p := range reg.Providers {
		p.Name = n
	}
	return &reg, reg.Validate()
}
