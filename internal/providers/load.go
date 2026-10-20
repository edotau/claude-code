package providers

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/edotau/claude-code/internal/paths"
)

//go:embed defaults.json
var defaultsJSON []byte

// Load merges the embedded defaults with the user overlay and validates the result.
func Load() (*Registry, error) {
	user, err := os.ReadFile(paths.ProvidersOverlay())
	if errors.Is(err, fs.ErrNotExist) {
		user = nil
	} else if err != nil {
		return nil, err
	}
	return Parse(defaultsJSON, user)
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
			return nil, fmt.Errorf("%s: %w", paths.ProvidersOverlay(), err)
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
