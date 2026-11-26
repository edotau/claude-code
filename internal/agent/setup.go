package agent

import (
	"context"
	"fmt"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// Check is one named check line; State: ok | fail | warn.
type Check struct {
	Name, State, Detail string
}

// RouteWarn is true when copilot's dispatch is not the route it is pinned to: no route, or a fallback off the pin.
func (s Schema) RouteWarn(ctx context.Context) bool {
	if s.Name != agentid.Copilot {
		return false
	}
	reg, err := providers.Load()
	if err != nil {
		return true
	}
	ri := s.DescribeRoute(ctx, reg)
	return !ri.OK || ri.Route != RouteCopilotLogin && ri.Provider != pinProfile(vault.GetAgentRecord(s.Name))
}

// configured is the no-network presence check: the agent binary, plus an opencode login for the cell's provider.
func configured(ctx context.Context, agent string, c router.Cell) error {
	s, ok := LookupSchema(agent)
	if !ok {
		return fmt.Errorf("unknown agent %s", agent)
	}
	if !s.External() {
		return nil
	}
	if _, err := paths.AgentBin(s.CLI.Binary); err != nil {
		return err
	}
	return s.Eng().Presence(ctx, c)
}
