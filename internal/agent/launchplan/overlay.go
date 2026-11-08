package launchplan

import (
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
)

// ApplyOverlay layers r under req's --env (the shorthand wins) and unsets $COWORKER_OVERLAY in the child (one hop).
func ApplyOverlay(req agent.RunOptions, r agentconfig.Resolved) agent.RunOptions {
	if agent.OverlayIdle(r) {
		return req
	}
	env := make([]string, 0, len(r.Set)+len(req.Env))
	for _, k := range slices.Sorted(maps.Keys(r.Set)) {
		env = append(env, k+"="+r.Set[k])
	}
	req.Env = append(env, req.Env...)
	req.KeepEnv = slices.Concat(req.KeepEnv, r.Keep)
	req.Unset = slices.Concat(req.Unset, r.Unset, []string{agentconfig.OverlayVar})
	req.OverlaySet = slices.Sorted(maps.Keys(r.Set))
	return req
}

// EnableOverlay checks path and points this process and its --job relaunches at it (absolute).
func EnableOverlay(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := agentconfig.LoadOverlay(abs); err != nil {
		return err
	}
	return os.Setenv(agentconfig.OverlayVar, abs)
}
