package settings

import (
	"context"
	"errors"

	"github.com/edotau/claude-code/internal/providers"
)

// VendorRouter is the vendor routing the renders need; agent/vendors implements it (settings cannot import it).
type VendorRouter interface {
	ResolveTarget(reg *providers.Registry, name string, sel providers.Selection, explicit bool) (
		target providers.Target, note string, err error)
	Configure(ctx context.Context, name string, target providers.Target, headlessPrompt string) (
		env, argv []string, err error)
	// OpencodeOverlayModels returns the overlay's model/small_model; ok false keeps the template's values.
	OpencodeOverlayModels() (mainModel, smallModel string, ok bool)
}

var errNoRouter = errors.New("settings: Options.Router is nil")
