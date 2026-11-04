package gemini

import "github.com/edotau/claude-code/internal/agent"

func init() {
	agent.Register(agent.Agent{Row: row, Driver: geminiEngine{agent.NewBaseEngine("gemini")}})
}

// row is gemini's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "gemini", CLI: agent.CLI{
	Binary: "agy",
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, Autonomous: agent.Native, DebugFile: agent.Native, Direct: agent.Native, Env: agent.Native,
		SystemPrompt: agent.Emulated, AppendSystemPrompt: agent.Emulated, OutputSchema: agent.Native, Persona: agent.Emulated},
	Events: []string{agent.EventStart, agent.EventText, agent.EventProgress, agent.EventResult, agent.EventError}, Cost: true,
	ResumeFormat: "agy --conversation %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true, RefusesReadOnly: true,
	ConfigureHint: "sign in with `agy` (Google login)", DisplayName: "antigravity", LoginArgv: []string{"agy"},
	ExtraUsage: [][2]string{{"bridge", "ingest files for a large-context task"}},
}}
