package copilot

import (
	"context"
	"fmt"

	"github.com/edotau/claude-code/internal/providers"
)

// isGithubProfile is true for a registry row whose models come from the Copilot catalog, not a gateway listing.
func isGithubProfile(p *providers.Provider) bool { return p != nil && p.Provider == "github" }

// RefreshProfile re-seals one profile's snapshot by its registry row: github or Databricks.
func RefreshProfile(ctx context.Context, name string) error {
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	p := reg.Providers[name]
	switch {
	case isGithubProfile(p):
		_, err = RefreshCatalog(ctx)
		return err
	case p != nil && p.Preset == providers.PresetDatabricks:
		return RefreshCopilotModels(ctx, name)
	}
	return fmt.Errorf("%q is not a Databricks or github profile", name)
}

// DiscoverySource is where p's models are discovered from and, when discovery cannot run, why not.
func DiscoverySource(p *providers.Provider) (source, skip string) {
	source, skip = providers.DiscoverySource(p)
	if isGithubProfile(p) {
		skip = ProfileCatalogSupport(p)
	}
	return source, skip
}

// DiscoverProfileModels lists p's live models: the Copilot catalog for github rows, the gateway otherwise.
func DiscoverProfileModels(ctx context.Context, p *providers.Provider) ([]providers.ModelInfo, error) {
	if isGithubProfile(p) {
		return ListProfileModels(ctx, p)
	}
	return providers.DiscoverModels(ctx, p)
}
