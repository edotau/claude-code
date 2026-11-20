package pi

import (
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
)

// refuseRequest refuses what the guard cannot judge: an Ask rule (no one to ask) and a --env that replaces the guard's wiring.
func refuseRequest(req agent.RunOptions) error {
	if len(req.Permissions.Ask) > 0 {
		return fmt.Errorf("pi cannot enforce ask rule %s: the guard only allows or blocks; use --deny", req.Permissions.Ask[0])
	}
	for _, kv := range req.Env {
		k, _, _ := strings.Cut(kv, "=")
		if k == agentDirEnv || k == agent.DispatchPermissionsEnv || strings.HasPrefix(k, "HARNESS_PI_") {
			return fmt.Errorf("--env %s: pi's guard wiring owns it; drop it from --env", k)
		}
	}
	return nil
}
