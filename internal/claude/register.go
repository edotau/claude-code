package claude

import (
	"context"
	"github.com/edotau/claude-code/internal/agent/wire"
	"os"

	"github.com/edotau/claude-code/internal/agent"
)

func init() {
	agent.Register(agent.Agent{Row: row, Driver: claudeAgent, Launcher: claudeLauncher{}})
}

// row is claude's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "claude", CLI: agent.CLI{
	PromptVia: agent.PromptStdin,
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, NoTools: agent.Native, Autonomous: agent.Native, NoPersist: agent.Native, SafeMode: agent.Native, DebugFile: agent.Native,
		SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, Env: agent.Native, OutputSchema: agent.Native, Persona: agent.Native,
		Permissions: agent.Native},
	Events: []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventRetry, wire.EventResult, wire.EventError},
	Cost:   true, ConfigureHint: "run `claude-code configure agents`", LoginArgv: []string{"claude", "auth", "login"},
}}

// claudeLauncher implements Launcher over Claude: the plan's overlay is written, its env layered on the process env.
type claudeLauncher struct{}

func (claudeLauncher) Claude(ctx context.Context, o agent.ClaudeOptions, args []string) (agent.ClaudeLaunch, error) {
	plan, err := Claude(ctx, agent.LaunchOptions{Provider: o.Provider, Model: o.Model, KeepEnv: o.KeepEnv}, args)
	if err != nil {
		return agent.ClaudeLaunch{}, err
	}
	if err := plan.WriteFiles(); err != nil {
		return agent.ClaudeLaunch{}, err
	}
	return agent.ClaudeLaunch{Binary: plan.Binary, Argv: plan.Argv, Env: plan.Env(os.Environ()), Set: plan.Set}, nil
}
