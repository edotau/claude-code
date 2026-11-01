package codex

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	agent.RegisterRow(1, agent.Schema{Name: "codex", CLI: agent.CLI{
		Binary: "codex", Dialects: []string{providers.ProtocolResponses},
		RouterSuffix: map[string]string{providers.ProtocolResponses: "/" + providers.ProtocolResponses},
		Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
			Tools: agent.Native, Autonomous: agent.Native, NoPersist: agent.Native, SafeMode: agent.Native, Direct: agent.Native, Env: agent.Native,
			SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, OutputSchema: agent.Native, Persona: agent.Emulated},
		Events:       []string{agent.EventStart, agent.EventText, agent.EventToolUse, agent.EventToolResult, agent.EventResult, agent.EventError},
		ResumeFormat: "codex resume %s", ModelsProvider: "openai", LoginArgv: []string{"codex", "login"},
		ConfigureHint: "run `codex login` (ChatGPT) or `claude-code configure` → codex → API key",
	}})
	agent.RegisterFileOps("codex", agent.FileOps{Write: writeCodexConfig, LoadBaseline: LoadCodexBaseline,
		SaveBaseline: SaveCodexBaseline, Revert: restoreCodexConfig})
	agent.Register(codexEngine{agent.NewBaseEngine("codex")})
}
