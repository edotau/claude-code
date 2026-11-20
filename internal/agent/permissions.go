package agent

import (
	"errors"
	"fmt"
	"slices"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/permission"
)

// SafeModeDeny is the first Deny rule unenforceable flags under --safe-mode; ok is false outside safe mode.
func SafeModeDeny(req RunOptions, unenforceable func(permission.Rule) bool) (permission.Rule, bool) {
	if !req.SafeMode {
		return permission.Rule{}, false
	}
	for _, r := range req.Permissions.Deny {
		if unenforceable(r) {
			return r, true
		}
	}
	return permission.Rule{}, false
}

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

// withReadOnlyRules adds --read-only's deny rules and shell allow where the agent enforces them itself (Emulated).
func withReadOnlyRules(c Caps, p permission.Permissions) permission.Permissions {
	if c.ReadOnly != Emulated {
		return p
	}
	p.Deny = append(slices.Clone(p.Deny), permission.ReadOnlyDeny...)
	p.Allow = append(slices.Clone(p.Allow), permission.ReadOnlyAllow) // Bash only where ReadOnlyShell approves
	return p
}

// LaunchPermissions is name's default policy as checkRun builds it, minus the outer dispatch's inherited layer.
func LaunchPermissions(name string, readOnly bool) (permission.Permissions, error) {
	p, err := DefaultPermissions(name)
	if err != nil || !readOnly {
		return p, err
	}
	e, err := Lookup(name)
	if err != nil {
		return permission.Permissions{}, err
	}
	return withReadOnlyRules(e.Caps(), p), nil
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
