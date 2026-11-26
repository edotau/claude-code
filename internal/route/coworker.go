package route

import (
	"fmt"
	"slices"
	"strings"
)

// OverlayVar names the overlay (a file path, inline JSON or -) every dispatch this process starts reads.
const OverlayVar = "COWORKER_OVERLAY"

// CheckProvider refuses a confinement naming no known provider or a profile holding ':'; the zero route passes.
func (r Route) CheckProvider() error {
	if r.Provider == "" || slices.Contains(Providers, r.Provider) && !strings.Contains(r.Profile, ":") {
		return nil
	}
	// never quotes the value
	return fmt.Errorf("unknown provider (%s)", strings.Join(Providers, "|"))
}

// Join is provider[:profile] for display; a profile without a provider joins to "".
func Join(provider, profile string) string {
	if provider == "" || profile == "" {
		return provider
	}
	return provider + ":" + profile
}
