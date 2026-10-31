package agent

import (
	"context"

	"github.com/edotau/claude-code/internal/providers"
)

type copilotEngine struct{ baseEngine }

var _ engine = copilotEngine{}

func (copilotEngine) login(_ *providers.Provider, model string, h *Headless) (env, argv []string, err error) {
	return copilotLogin(model, h)
}

func (copilotEngine) wire(_ context.Context, r wireRoute, h *Headless) (env, argv []string, err error) {
	client := providers.Target{Provider: r.Target.Provider, Model: r.Target.Model, OneM: r.Target.OneM}.ClientModel()
	return copilotWire(r.Provider, r.Dialect, r.Base, r.Model, providers.ContextWindow(r.Target.Provider, client), r.Cred, r.Routed, h)
}

func (e copilotEngine) fallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	return routeFallback(reg, e.describeRoute(reg))
}

func (copilotEngine) describeRoute(reg *providers.Registry) RouteInfo {
	return describeCopilotRoute(reg)
}

func (copilotEngine) LoginStatus() (bool, string) {
	if a, ok := ResolveCopilotAuth(); ok {
		return true, a.Label()
	}
	return false, ""
}
