// routing.go — the "routing" page: every internal/agent agent module's fields, grouped by agent, edited
// and held until the page is left, then written to the providers overlay's routing object via
// providers.UpdateRouting.
package configure

import (
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
)

// ValidateRoutingInput runs f.Set against a scratch Routing, so a bad value is reported before staging.
func ValidateRoutingInput(f settings.RoutingField, input string) error {
	var scratch providers.Routing
	return f.Set(&scratch, input)
}
