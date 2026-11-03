package copilot

import (
	"context"
	"errors"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

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
