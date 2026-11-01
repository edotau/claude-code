package agent

import (
	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	RegisterRow(4, Schema{Name: "copilot", CLI: CLI{
		Binary: "copilot", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI},
		RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI,
			providers.ProtocolResponses: "/" + providers.ProtocolResponses},
		MinVersion: copilotsdk.MinMultiProviderCLI, MinReason: "named-provider model registry needs it",
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, ReadOnly: Native, Autonomous: Native, Memory: Native, Direct: Native, Env: Native,
			SystemPrompt: Emulated, AppendSystemPrompt: Emulated, OutputSchema: Emulated, Persona: Emulated},
		Events:       []string{EventStart, EventText, EventResult, EventError},
		ResumeFormat: "copilot --resume %s", ModelsProvider: "github", ReadNeedsTools: true,
		LoginArgv:     []string{"copilot", "login"},
		ConfigureHint: "run `claude-code configure github` (Copilot) or set an Anthropic/Gemini key",
	}})
	Register(copilotAgent)
}
