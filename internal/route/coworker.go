package route

import (
	"fmt"
	"slices"
	"strings"
)

// OverlayVar names the overlay (a file path, inline JSON or -) every dispatch this process starts reads.
const OverlayVar = "COWORKER_OVERLAY"

// Parse splits a provider[:profile] value into its provider and optional profile; "" is no confinement.
func Parse(v string) (provider, profile string, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", "", nil
	}
	if r := ParseLegacy(v); r.Provider != "" {
		return r.Provider, r.Profile, nil
	}
	provider, profile, named := strings.Cut(v, ":")
	if named && profile == "" && slices.Contains(Providers, provider) {
		return "", "", fmt.Errorf("empty profile: want %s or %s:<profile>", provider, provider)
	}
	// never quotes v
	return "", "", fmt.Errorf("unknown provider (%s)", strings.Join(Providers, "|"))
}

// Join is Parse's inverse: provider[:profile]; a profile without a provider joins to "".
func Join(provider, profile string) string {
	if provider == "" || profile == "" {
		return provider
	}
	return provider + ":" + profile
}
