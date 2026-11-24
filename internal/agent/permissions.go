package agent

import (
	"errors"

	"github.com/edotau/claude-code/internal/permission"
)

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
