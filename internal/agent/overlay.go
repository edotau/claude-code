// overlay.go — folds a resolved COWORKER_OVERLAY (agentconfig) into a headless RunOptions or an interactive Plan.
package agent

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
)

// OverlayIdle reports a launch the overlay changes nothing about: empty layer and no $COWORKER_OVERLAY to hide.
func OverlayIdle(r agentconfig.Resolved) bool {
	return len(r.Set)+len(r.Unset)+len(r.Keep) == 0 && os.Getenv(agentconfig.OverlayVar) == ""
}

// CheckRouteNames refuses a name in names that route (KEY=VALUE, before the overlay is appended) already sets.
func CheckRouteNames(route, names []string) error {
	own := map[string]bool{}
	for _, kv := range route {
		k, _, _ := strings.Cut(kv, "=")
		own[k] = true
	}
	for _, n := range names {
		if own[n] {
			return fmt.Errorf("overlay set %s: set by the launch for its route; use --profile", n)
		}
	}
	return nil
}

// CheckOverlay refuses an overlay that sets or unsets a name the plan's launch sets for its route.
func (p Plan) CheckOverlay(r agentconfig.Resolved) error {
	for _, k := range slices.Sorted(maps.Keys(r.Set)) {
		if _, own := p.Set[k]; own {
			return fmt.Errorf("overlay set %s: set by the launch for its route; use --profile", k)
		}
	}
	for _, k := range r.Unset {
		if _, own := p.Set[k]; own {
			return fmt.Errorf("overlay unset %s: set by the launch for its route; use --profile", k)
		}
	}
	return nil
}

// WithOverlay layers r over an interactive plan the way ApplyOverlay does for a headless run; the launch's own Set wins.
func (p Plan) WithOverlay(r agentconfig.Resolved) Plan {
	if OverlayIdle(r) {
		return p
	}
	set := maps.Clone(r.Set)
	if set == nil {
		set = map[string]string{}
	}
	maps.Copy(set, p.Set)
	p.Set, p.Keep = set, slices.Concat(p.Keep, r.Keep)
	unset := slices.DeleteFunc(slices.Clone(r.Unset), func(k string) bool { _, own := p.Set[k]; return own })
	p.Unset = slices.Concat(p.Unset, unset, []string{agentconfig.OverlayVar})
	return p
}
