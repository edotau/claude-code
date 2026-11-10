package agent

import (
	"errors"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// firstOutputDeadline is the free-dispatch stall deadline from the registry, else OpencodeRouting's 90s floor.
func firstOutputDeadline() time.Duration {
	reg, err := providers.Load()
	if err != nil {
		return (providers.OpencodeRouting{}).FirstOutput()
	}
	return reg.Routing.Opencode.FirstOutput()
}

// ErrStalled is the sentinel a dispatch's Run returns when its stream went silent past the stall deadline: a
// fatal, unwrapped failure, so Do never retries it but dispatchUnavailable falls through to the chain's backup dispatch.
var ErrStalled = errors.New("dispatch stalled: no output within the stall deadline")

// HubEngine is one hub ladder step: an Engine that names the provider account answering it, so every dispatch of a
// chain reads the same Result.Agent and only Result.Provider shows which account did.
type HubEngine interface {
	Engine
	HubProvider() *providers.Provider
	// WithFirstOutput returns the step failing with ErrStalled when nothing streams within d.
	WithFirstOutput(d time.Duration) Engine
}

// Hub is the opencode-routed dispatch ladder, registered by internal/opencode.
type Hub struct {
	// Start resolves the provider a hub agent's first dispatch targets (explicitProvider forces one).
	Start func(reg *providers.Registry, agentName, explicitProvider string) (*providers.Provider, error)
	// Dispatch builds the hub step for agentName on provider; reg resolves a provider:model override.
	Dispatch func(agentName string, provider *providers.Provider, reg *providers.Registry) HubEngine
	// OwnPlan reports whether an opencodeID is opencode's own plan (no registry provider, billed as a subscription).
	OwnPlan func(opencodeID string) bool
}

var hub *Hub

// IsHubAgent reports whether name is one ask routes through opencode unless --native is set. codex and
// copilot run their own CLI first (user decision): the hub cannot pass copilot's `auto` or its pinned account.
func IsHubAgent(name string) bool {
	return slices.ContainsFunc(rows(), func(s Schema) bool { return s.Name == name && s.CLI.Hub })
}

// HubStart resolves the provider a hub agent's first dispatch targets.
func HubStart(reg *providers.Registry, agentName, explicitProvider string) (*providers.Provider, error) {
	return hub.Start(reg, agentName, explicitProvider)
}

// NewHubDispatch builds a hub dispatch Agent for agentName on provider; reg resolves a provider:model override.
func NewHubDispatch(agentName string, provider *providers.Provider, reg *providers.Registry) Engine {
	return hub.Dispatch(agentName, provider, reg)
}
