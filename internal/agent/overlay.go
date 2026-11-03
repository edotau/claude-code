// overlay.go — folds a resolved COWORKER_OVERLAY (agentconfig) into a headless RunOptions or an interactive Plan.
package agent

import (
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/agentconfig"
)

// overlayIdle reports a launch the overlay changes nothing about: empty layer and no $COWORKER_OVERLAY to hide.
func overlayIdle(r agentconfig.Resolved) bool {
	return len(r.Set)+len(r.Unset)+len(r.Keep) == 0 && os.Getenv(agentconfig.OverlayVar) == ""
}

// ApplyOverlay layers r under req's --env (the shorthand wins) and unsets $COWORKER_OVERLAY in the child (one hop).
func ApplyOverlay(req RunOptions, r agentconfig.Resolved) RunOptions {
	if overlayIdle(r) {
		return req
	}
	env := make([]string, 0, len(r.Set)+len(req.Env))
	for _, k := range slices.Sorted(maps.Keys(r.Set)) {
		env = append(env, k+"="+r.Set[k])
	}
	req.Env = append(env, req.Env...)
	req.KeepEnv = slices.Concat(req.KeepEnv, r.Keep)
	req.Unset = slices.Concat(req.Unset, r.Unset, []string{agentconfig.OverlayVar})
	return req
}

// WithOverlay layers r over an interactive plan the way ApplyOverlay does for a headless run; set wins over the plan's.
func (p Plan) WithOverlay(r agentconfig.Resolved) Plan {
	if overlayIdle(r) {
		return p
	}
	set := maps.Clone(p.Set)
	if set == nil {
		set = map[string]string{}
	}
	maps.Copy(set, r.Set)
	p.Set, p.Keep = set, slices.Concat(p.Keep, r.Keep)
	p.Unset = slices.Concat(p.Unset, r.Unset, []string{agentconfig.OverlayVar})
	return p
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
