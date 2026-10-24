package hooks

import "github.com/edotau/claude-code/internal/inflight"

// sessionOwnerPid is the Claude Code process behind this hook: settings.json runs hooks via `sh -c`, so skip shells.
func sessionOwnerPid() int { return inflight.SessionOwner() }
