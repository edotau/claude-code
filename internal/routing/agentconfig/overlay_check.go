// overlay_check.go — Overlay validation: the rules a parsed overlay must pass before a dispatch uses it.
package agentconfig

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/edotau/claude-code/internal/remote/network/remote"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
)

func (o Overlay) validate(src overlaySrc) error {
	if o.TokenCommand != nil && len(*o.TokenCommand) > 0 && strings.TrimSpace((*o.TokenCommand)[0]) == "" {
		return errors.New("tokenCommand: needs an executable")
	}
	if len(o.Files) > 0 { // present at all: [] and null too
		return errors.New("files: not supported yet")
	}
	if err := cmp.Or(o.checkRouting(), o.checkProviderMode(), o.checkRun()); err != nil {
		return err
	}
	keep, err := o.checkKeep(src)
	if err != nil {
		return err
	}
	if err := o.checkSet(src, keep); err != nil {
		return err
	}
	return o.checkUnset(src, keep)
}

// checkProviderMode wants each key a Claude provider of the registry (when it loads) and each value a mode.
func (o Overlay) checkProviderMode() error {
	if len(o.ProviderMode) == 0 {
		return nil
	}
	reg, err := providers.LoadShared()
	if err != nil {
		return errors.New("providerMode: the provider registry does not load, so its providers cannot be checked")
	}
	var names []string
	for _, p := range router.ClaudeProviders(reg) {
		names = append(names, p.Name)
	}
	for _, name := range slices.Sorted(maps.Keys(o.ProviderMode)) {
		if !slices.Contains(names, name) {
			return errors.New("providerMode: unknown provider (" + strings.Join(names, "|") + ")")
		}
		if _, err := router.ParseProviderMode(o.ProviderMode[name]); err != nil || o.ProviderMode[name] == "" {
			return errors.New("providerMode: " + router.ProviderModeError{}.Error()) // empty is no mode: the schema agrees
		}
	}
	return nil
}

// ErrRemoteWithHost refuses remote on a run that goes to a host.
var ErrRemoteWithHost = errors.New("remote with a host: drop one (the far machine ignores remote)")

// checkRouting applies routeKeys' rules in order, then the cross-key ones; an error names the key, never its value.
func (o Overlay) checkRouting() error {
	if err := o.validateBudgetValues(); err != nil {
		return err
	}
	for _, k := range routeKeys {
		if err := k.check(k.get(o)); err != nil {
			return err
		}
	}
	return cmp.Or(route.OneOf("fallback_mode", o.Fallback, route.FallbackModes), route.OneOf("effort", o.Effort, agentid.EffortNames()),
		route.OneOf("runner", o.Runner, route.Runners), o.checkCrossKeys())
}

func (k routeKey) check(v string) error {
	switch {
	case v == "":
	case strings.TrimSpace(v) == "" || strings.ContainsFunc(v, unicode.IsControl):
		return errors.New(k.name + ": blank or holds a control character")
	case k.ok != nil && !k.ok(v):
		return errors.New(k.name + ": " + k.msg)
	}
	return nil
}

// checkCrossKeys applies the rules spanning several routing keys: a route names provider and profile together.
func (o Overlay) checkCrossKeys() error {
	if err := cmp.Or(o.checkRouteUnit(), o.checkRouteScope()); err != nil {
		return err
	}
	switch {
	case strings.ContainsFunc(o.Profile, func(r rune) bool { return r == ':' || unicode.IsSpace(r) }):
		return errors.New("profile: want a name without ':' or spaces")
	case o.Remote != "" && o.Host != "" && !remote.IsSelf(o.Host):
		return ErrRemoteWithHost
	}
	return nil
}

// errEnvName refuses a malformed name; nameErr never echoes it (it may be a pasted token).
var errEnvName = errors.New("want an env-style name (letters, digits, _; not starting with a digit)")

func nameErr(label, name string, err error) error {
	if err == errEnvName {
		return fmt.Errorf("%s: %w", label, err)
	}
	return fmt.Errorf("%s %s: %w", label, name, err)
}

func (o Overlay) checkKeep(src overlaySrc) (map[string]bool, error) {
	keep := map[string]bool{} // upper-cased: set, keep and unset names overlap whatever their case
	for _, n := range o.Keep {
		if err := nameRule(keepRules, n); err != nil {
			return nil, nameErr(src.keep, n, err)
		}
		keep[strings.ToUpper(n)] = true
	}
	return keep, nil
}

func (o Overlay) checkSet(src overlaySrc, keep map[string]bool) error {
	route := providers.RouteSecretGuard()
	for _, n := range slices.Sorted(maps.Keys(o.Set)) {
		if err := setRule(src.setRules, n, o.Set[n], keep, route); err != nil {
			return nameErr(src.set, n, err)
		}
	}
	return nil
}

func (o Overlay) checkUnset(src overlaySrc, keep map[string]bool) error {
	taken := maps.Clone(keep)
	for n := range o.Set {
		taken[strings.ToUpper(n)] = true
	}
	for _, n := range o.Unset {
		err := nameRule(unsetRules, n)
		if err == nil && taken[strings.ToUpper(n)] {
			err = errors.New("also set or kept")
		}
		if err != nil {
			return nameErr(src.unset, n, err)
		}
	}
	return nil
}

// checkRouteUnit refuses a partial route: a profile, identity or account needs its provider, a provider its profile.
func (o Overlay) checkRouteUnit() error {
	switch {
	case o.Provider == "" && (o.Endpoint != "" || o.Profile != "" || o.Identity != "" || o.Account != ""):
		return errors.New("provider: required with profile, endpoint, identity or account")
	case o.Provider != "" && o.Profile == "":
		return errors.New("profile: required with provider (a registry row, or auto)")
	case o.Endpoint != "":
		return route.Route{Endpoint: o.Endpoint, Provider: o.Provider, Profile: o.Profile}.Validate()
	}
	return nil
}

// checkRouteScope refuses identity off Databricks and account off GitHub.
func (o Overlay) checkRouteScope() error {
	switch {
	case o.Identity != "" && o.Provider != "databricks":
		return errors.New("identity: databricks only")
	case o.Account != "" && o.Provider != "github":
		return errors.New("account: github only")
	}
	return nil
}
