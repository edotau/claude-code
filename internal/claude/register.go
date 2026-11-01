package claude

import "github.com/edotau/claude-code/internal/agent"

func init() {
	agent.Register(agent.Agent{Row: row, Driver: claudeAgent, Launcher: claudeLauncher{}})
}

// row is claude's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "claude", CLI: agent.CLI{
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, NoTools: agent.Native, Autonomous: agent.Native, NoPersist: agent.Native, SafeMode: agent.Native, DebugFile: agent.Native,
		SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, Env: agent.Native, OutputSchema: agent.Native, Persona: agent.Native},
	Events: []string{agent.EventStart, agent.EventText, agent.EventToolUse, agent.EventRetry, agent.EventResult, agent.EventError},
	Cost:   true, ConfigureHint: "run `claude-code configure agents`", LoginArgv: []string{"claude", "auth", "login"},
}}
