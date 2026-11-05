package codex

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	agent.Register(agent.Agent{Row: row, Driver: codexEngine{agent.NewBaseEngine("codex")}})
	render.RegisterFileOps("codex", render.FileOps{Write: writeCodexConfig, LoadBaseline: LoadCodexBaseline,
		SaveBaseline: SaveCodexBaseline, Revert: restoreCodexConfig})
	render.RegisterFileOps("codex-hooks", render.FileOps{Write: writeCodexHooks})
}

// row is codex's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "codex", CLI: agent.CLI{
	Binary: "codex", Dialects: []string{providers.ProtocolResponses},
	RouterSuffix: map[string]string{providers.ProtocolResponses: "/" + providers.ProtocolResponses},
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, Autonomous: agent.Native, NoPersist: agent.Native, SafeMode: agent.Native, Direct: agent.Native, Env: agent.Native,
		SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, OutputSchema: agent.Native, Persona: agent.Emulated,
		Permissions: agent.Native},
	Events:       []string{agent.EventStart, agent.EventText, agent.EventToolUse, agent.EventToolResult, agent.EventResult, agent.EventError},
	ResumeFormat: "codex resume %s", ModelsProvider: "openai", LoginArgv: []string{"codex", "login"},
	ConfigureHint: "run `codex login` (ChatGPT) or `claude-code configure` → codex → API key",
}}
