package statusline

import (
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// tierTTL caps the ~/.claude.json parse at one per minute across renders.
const tierTTL = time.Minute

// claudeTier is the account's rate-limit tier from ~/.claude.json (user before org), cached for tierTTL.
func claudeTier() string {
	stamp := stampFile("tier", paths.ClaudeJSON())
	if f := freshStamp(stamp, tierTTL, 1); f != nil {
		return f[0]
	}
	tier := providers.ClaudeAccount().Tier()
	writeTimedStamp(stamp, tier)
	return tier
}
