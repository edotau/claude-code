package statusline

import (
	"fmt"

	"github.com/edotau/claude-code/internal/providers"
)

var anthropicLabeler = labeler{provider: anthropicProvider, model: modelSegment, auth: anthropicAuth}

// anthropicProvider names the profile "ultra" for the claude.ai login profile on a 20x account.
func anthropicProvider(p *providers.Provider) string {
	if p.Auth.Type == providers.AuthCLI && providers.MaxTierName(claudeTier()) == "Ultra" {
		return kindLabel(p, "ultra")
	}
	return kindLabel(p, p.Name)
}

// anthropicAuth is "7d:N%" on the cli (subscription) login, from the payload's rate_limits, else the usage cache.
func anthropicAuth(in Input, session string, p *providers.Provider) string {
	if p.Auth.Method() != providers.AuthCLI {
		return tokenAuth(in, session, p)
	}
	week := limitsFromPayload(in)
	if week < 0 {
		week = limitsFromCache()
	}
	if week < 0 {
		return ""
	}
	return fmt.Sprintf("7d:%d%%", week)
}
