package pi

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/settings"
)

// refuseRequest refuses what the guard cannot judge: an Ask rule (no one to ask) and a --env that replaces the guard's wiring.
func refuseRequest(req agent.RunOptions) error {
	if len(req.Permissions.Ask) > 0 {
		return fmt.Errorf("pi cannot enforce ask rule %s: the guard only allows or blocks; use --deny", req.Permissions.Ask[0])
	}
	for _, kv := range req.Env {
		k, _, _ := strings.Cut(kv, "=")
		if guardWiring(k) {
			return fmt.Errorf("--env %s: pi's guard wiring owns it; drop it from --env", k)
		}
		if settings.IsHookControlEnv(k) {
			return fmt.Errorf("--env %s: a hook-control variable could turn pi's guard off; drop it from --env", k)
		}
	}
	for _, k := range req.OverlaySet {
		if guardWiring(k) {
			return fmt.Errorf("overlay set %s: pi's guard wiring owns it; drop it from the overlay", k)
		}
		if settings.IsHookControlEnv(k) {
			return fmt.Errorf("overlay set %s: a hook-control variable could turn pi's guard off; drop it from the overlay", k)
		}
	}
	return nil
}

// guardWiring is an env name the pi guard sets itself; neither --env nor an overlay may replace it.
func guardWiring(k string) bool {
	return k == agentDirEnv || k == agent.DispatchPermissionsEnv || strings.HasPrefix(k, "HARNESS_PI_")
}

// withoutHookControl drops every hook-control variable from a child env: pi's own process never inherits one.
func withoutHookControl(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return settings.IsHookControlEnv(k)
	})
}
