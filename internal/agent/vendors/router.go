package vendors

import (
	"context"

	"github.com/edotau/claude-code/internal/providers"
)

// Router is the settings.VendorRouter backed by this package's routing.
type Router struct{}

func (Router) ResolveTarget(reg *providers.Registry, name string, sel providers.Selection, explicit bool) (providers.Target, string, error) {
	return ResolveTarget(reg, name, sel, explicit)
}

func (Router) Configure(ctx context.Context, name string, target providers.Target, headlessPrompt string) ([]string, []string, error) {
	return Configure(ctx, name, target, headlessPrompt)
}

func (Router) OpencodeOverlayModels() (string, string, bool) { return opencodeOverlayModels() }
