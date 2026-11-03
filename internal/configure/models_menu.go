// models_menu.go — model discovery + selection: one row per alias (current pin, use case), picking one lists the
// profile's served models and pins the choice; reached from `configure models` and the profile/login flow.
package configure

import (
	"context"

	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/providers"
)

// CatalogForAlias lists live choices plus saved additions, with an explained offline fallback.
func CatalogForAlias(ctx context.Context, reg *providers.Registry, p *providers.Provider, alias string) (choices []providers.ModelInfo, note string) {
	defer func() {
		if p.Provider == "github" {
			return // Saved history must not bypass Copilot policy filtering.
		}
		saved, err := providers.PickerModels(p)
		if err != nil {
			note += " saved picker additions unavailable: " + err.Error()
			return
		}
		choices = providers.MergePickerModels(choices, saved)
	}()
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
	infos, note, err = providers.FallbackModels(p, err)
	if err != nil {
		return nil, "live catalog unavailable (" + err.Error() + ") and no fallback for the " + alias + " alias"
	}
	return infos, note
}
