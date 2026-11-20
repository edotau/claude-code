package agentconfig

import (
	"errors"

	"github.com/edotau/claude-code/internal/route"
)

// Route is the overlay's route unit, taken whole by route.Pick; the keys stay flat fields so the wire shape is fixed.
func (o Overlay) Route() route.Route {
	return route.Route{Provider: o.Provider, Profile: o.Profile, Identity: o.Identity, Account: o.Account,
		Host: o.Host, Remote: o.Remote}
}

// WithoutRoute is o with the route unit's provider, profile, identity and account cleared; host and remote stay.
func (o Overlay) WithoutRoute() Overlay {
	o.Provider, o.Profile, o.Identity, o.Account = "", "", "", ""
	return o
}

// checkRouteUnit refuses a partial route: a profile, identity or account needs its provider, a provider its profile.
func (o Overlay) checkRouteUnit() error {
	switch {
	case o.Provider == "" && (o.Profile != "" || o.Identity != "" || o.Account != ""):
		return errors.New("provider: required with profile, identity or account")
	case o.Provider != "" && o.Profile == "":
		return errors.New("profile: required with provider (a registry row, or auto)")
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
