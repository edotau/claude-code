// models_menu.go — model discovery + selection: one row per alias (current pin, use case), picking one lists the
// profile's served models and pins the choice; reached from `configure models` and the profile/login flow.
package configure

import (
	"context"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/providers"
)

// CatalogForAlias is alias's pickable catalog: the provider's live models, or — when that listing fails
// in a known way (FallbackModels) — a known offline list, with a note explaining the substitution.
func CatalogForAlias(ctx context.Context, reg *providers.Registry, p *providers.Provider, alias string) ([]providers.ModelInfo, string) {
	var infos []providers.ModelInfo
	var err error
	if p.Provider == "github" {
		infos, err = copilot.ListModels(ctx)
	} else {
		infos, err = providers.ListModels(ctx, reg, p)
	}
	if err == nil {
		return infos, ""
	}
	infos, note, err := providers.FallbackModels(p, err)
	if err != nil {
		return nil, "live catalog unavailable (" + err.Error() + ") and no fallback for the " + alias + " alias"
	}
	return infos, note
}
