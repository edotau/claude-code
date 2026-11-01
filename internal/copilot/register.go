package copilot

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	agent.RegisterRow(4, agent.Schema{Name: "copilot", CLI: agent.CLI{
		Binary: "copilot", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI},
		RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI,
			providers.ProtocolResponses: "/" + providers.ProtocolResponses},
		MinVersion: MinMultiProviderCLI, MinReason: "named-provider model registry needs it",
		Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
			Tools: agent.Native, ReadOnly: agent.Native, Autonomous: agent.Native, Memory: agent.Native, Direct: agent.Native, Env: agent.Native,
			SystemPrompt: agent.Emulated, AppendSystemPrompt: agent.Emulated, OutputSchema: agent.Emulated, Persona: agent.Emulated},
		Events:       []string{agent.EventStart, agent.EventText, agent.EventResult, agent.EventError},
		ResumeFormat: "copilot --resume %s", ModelsProvider: "github", ReadNeedsTools: true,
		LoginArgv:     []string{"copilot", "login"},
		ConfigureHint: "run `claude-code configure github` (Copilot) or set an Anthropic/Gemini key",
	}})
	agent.Register(copilotAgent)
}
