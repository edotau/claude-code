package agentconfig

import (
	"errors"

	"github.com/edotau/claude-code/internal/route"
)

// routeUnit is route.Route embedded flat, so its keys keep their wire names and promote onto Overlay.
type routeUnit = route.Route

// Route is the overlay's route unit, taken whole by route.Pick.
func (o Overlay) Route() route.Route { return o.routeUnit }

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
