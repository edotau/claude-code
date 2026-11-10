package launchplan

import (
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// envMap turns a KEY=VALUE env list into a map, dropping malformed entries.
func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// lookPath is the seam for resolving an agent binary.
var lookPath = paths.AgentBin
