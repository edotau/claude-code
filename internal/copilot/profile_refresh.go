package copilot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
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

// ProfileCatalogSupport requires an explicit account for discovery without launch-account fallback.
func ProfileCatalogSupport(p *providers.Provider) string {
	if strings.TrimSpace(p.Login) == "" {
		return "Copilot discovery requires an explicit profile login; implicit launch-account fallback is disabled"
	}
	return ""
}

// ListProfileModels uses only the requested account, never global pin switching.
func ListProfileModels(ctx context.Context, p *providers.Provider) ([]providers.ModelInfo, error) {
	if note := ProfileCatalogSupport(p); note != "" {
		return nil, errors.New(note)
	}
	bin, err := paths.AgentBin("copilot")
	if err != nil {
		return nil, errors.New("copilot CLI unavailable")
	}
	token := GHToken(p.Login)
	if token == "" {
		return nil, errors.New("requested GitHub account has no gh token")
	}
	list, err := SDKListModels(ctx, CatalogOptions(bin, token))
	if err != nil {
		return nil, errors.New("requested account Copilot catalog unavailable")
	}
	var out []providers.ModelInfo
	for _, m := range list {
		if !m.Enabled {
			continue
		}
		out = append(out, providers.ModelInfo{ID: m.ID, Name: m.Name, UseCase: models.UseCase(m.ID, m.Name), Capabilities: copilotCapabilities(m)})
	}
	return out, nil
}
