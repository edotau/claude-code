package launch

import (
	"context"
	"os"

	"github.com/edotau/claude-code/internal/agent"
)

// Launcher implements agent.Launcher over Claude: the plan's overlay is written, its env layered on the process env.
type Launcher struct{}

func (Launcher) Claude(ctx context.Context, o agent.ClaudeOptions, args []string) (agent.ClaudeLaunch, error) {
	plan, err := Claude(ctx, Options{Provider: o.Provider, Model: o.Model, KeepEnv: o.KeepEnv}, args)
	if err != nil {
		return agent.ClaudeLaunch{}, err
	}
	if err := plan.WriteOverlay(); err != nil {
		return agent.ClaudeLaunch{}, err
	}
	return agent.ClaudeLaunch{Binary: plan.Binary, Argv: plan.Argv, Env: plan.Env(os.Environ()), Set: plan.Set}, nil
}
