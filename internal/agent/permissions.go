package agent

import (
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/permission"
)

// DefaultPermissions is name's default dispatch policy, the first layer permission.Resolve applies.
func DefaultPermissions(name string) (permission.Permissions, error) {
	r, err := agentconfig.Settings(name)
	if err == nil {
		var p permission.Permissions
		if p, err = policyPermissions(r.Permissions); err == nil {
			return p, nil
		}
	}
	return permission.Permissions{}, fmt.Errorf("%s default permissions: %w", name, err)
}

// refusePermissions fails closed when req carries Deny rules (or plan mode) and the agent cannot read Permissions.
func refusePermissions(c Caps, agentName string, p permission.Permissions) error {
	if c.Permissions != None || (len(p.Deny) == 0 && p.Mode != permission.PermPlan) {
		return nil
	}
	what := "--permissions plan"
	if len(p.Deny) > 0 {
		what = "permission deny rules"
	}
	return errors.New(agentName + " cannot enforce " + what + " yet; dispatch refused")
}
