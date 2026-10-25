package vendoragent

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/crossgen"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/paths"
)

// Check is one `claude-code <v> setup` line.
type Check struct {
	Name, State, Detail string // State: ok | fail | warn
}

// Report is the full setup result for one vendor.
type Report struct {
	Vendor string
	Checks []Check
	OK     bool // binary ok && login ok
}

// Setup runs the binary, login, route and projection checks for a vendor. Text rendering is the caller's job.
func Setup(name string) Report {
	a, ok := Lookup(name)
	if !ok {
		return Report{Vendor: name, Checks: []Check{{Name: "vendor", State: "fail", Detail: errUnknownVendor(name).Error()}}}
	}
	binCheck := binaryCheck(a)
	loginCheck := loginCheck(a)
	rep := Report{
		Vendor: name,
		Checks: []Check{binCheck, loginCheck, routeCheck(name), projectionCheck(name)},
		OK:     binCheck.State == "ok" && loginCheck.State == "ok",
	}
	return rep
}

// binaryCheck looks up the vendor executable via paths.VendorBin.
func binaryCheck(a Adapter) Check {
	p, err := paths.VendorBin(a.Name)
	if err != nil {
		return Check{Name: "binary", State: "fail", Detail: err.Error()}
	}
	return Check{Name: "binary", State: "ok", Detail: p}
}

// loginCheck runs the adapter's own-login probe.
func loginCheck(a Adapter) Check {
	if a.Login == nil || !a.Login() {
		return Check{Name: "login", State: "fail", Detail: "no " + a.Name + " login found (claude-code vault set / vendor's own login)"}
	}
	return Check{Name: "login", State: "ok", Detail: a.Name + " login present"}
}

// routeCheck pulls the "<name> route" detail from launch.RouteDetail, without running the full doctor sweep.
func routeCheck(name string) Check {
	detail, ok := launch.RouteDetail(name)
	if !ok {
		return Check{Name: "route", State: "warn", Detail: "no route check found for " + name}
	}
	return Check{Name: "route", State: "ok", Detail: detail}
}

// projectionCheck reports whether state/projections/<vendor>/agents has any rendered agents.
func projectionCheck(name string) Check {
	dir := crossgen.ProjectionDir(paths.ConfigDir(), name)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return Check{Name: "projection", State: "warn", Detail: fmt.Sprintf("no rendered agents at %s (claude-code agents sync)", dir)}
	}
	return Check{Name: "projection", State: "ok", Detail: fmt.Sprintf("%d agent(s) at %s", len(entries), dir)}
}
