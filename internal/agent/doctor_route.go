package agent

import (
	"github.com/edotau/claude-code/internal/providers"
)

// RouteDetail returns the "<agent> route" detail string DoctorChecks would emit for the agent, without
// running the rest of the doctor sweep; ok is false for an unknown agent or a registry load error.
func RouteDetail(name string) (detail string, ok bool) {
	return schemaFor(name).eng().RouteDetail()
}

// LoadSelection is the registry and its unpinned selection; ok is false when the registry does not load.
func LoadSelection() (reg *providers.Registry, sel providers.Selection, selErr error, ok bool) {
	reg, err := providers.Load()
	if err != nil || reg == nil {
		return nil, providers.Selection{}, nil, false
	}
	sel, selErr = providers.Select(reg, "")
	return reg, sel, selErr, true
}
