package agent

import (
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	RegisterRow(1, Schema{Name: "codex", CLI: CLI{
		Binary: "codex", Dialects: []string{providers.ProtocolResponses},
		RouterSuffix: map[string]string{providers.ProtocolResponses: "/" + providers.ProtocolResponses},
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, Autonomous: Native, NoPersist: Native, SafeMode: Native, Direct: Native, Env: Native,
			SystemPrompt: Native, AppendSystemPrompt: Native, OutputSchema: Native, Persona: Emulated},
		Events:       []string{EventStart, EventText, EventToolUse, EventToolResult, EventResult, EventError},
		ResumeFormat: "codex resume %s", ModelsProvider: "openai", LoginArgv: []string{"codex", "login"},
		ConfigureHint: "run `codex login` (ChatGPT) or `claude-code configure` → codex → API key",
	}})
	Register(codexEngine{BaseEngine{"codex"}})
}
