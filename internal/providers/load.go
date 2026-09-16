package providers

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

// UserFile is the per-user registry overlay: providers replace built-ins by name; default/fallback override when set.
func UserFile() string { return filepath.Join(paths.ConfigDir(), "providers.json") }

// Load merges the embedded defaults with UserFile and validates the result.
func Load() (*Registry, error) {
	user, err := os.ReadFile(UserFile())
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
			return nil, fmt.Errorf("%s: %w", UserFile(), err)
		}
		if o.Default != "" {
			reg.Default = o.Default
		}
		if o.Fallback != nil {
			reg.Fallback = o.Fallback
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
