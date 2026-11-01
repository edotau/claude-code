package opencode

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	agent.RegisterRow(3, agent.Schema{Name: "opencode", CLI: agent.CLI{
		Binary: "opencode", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolGemini, providers.ProtocolOpenAI},
		RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolGemini: "/" + providers.ProtocolGemini,
			providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI, providers.ProtocolResponses: "/" + providers.ProtocolResponses},
		MaxVersion: "1.x", MaxReason: "2.x broke the harness opencode route", // keep in step with opencodeMaxMajor
		Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
			Tools: agent.Native, Autonomous: agent.Native, SafeMode: agent.Native, Direct: agent.Native, Env: agent.Native,
			SystemPrompt: agent.Native, AppendSystemPrompt: agent.Emulated, OutputSchema: agent.Emulated, Persona: agent.Emulated},
		Events:       []string{agent.EventStart, agent.EventText, agent.EventToolUse, agent.EventToolResult, agent.EventRetry, agent.EventResult, agent.EventError},
		ResumeFormat: "opencode run --session %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
		PromptInArgv: true, DirFlag: true, Hub: true, LoginArgv: []string{"opencode", "auth", "login"},
		ConfigureHint: "run `opencode auth login` (opencode Go) or `claude-code configure github` (Copilot)",
		CatalogUsage:  "model menu: list|add|remove|import|reset|check|sync",
		CatalogHint:   "Serving-only discovery: claude-code opencode catalog sync --profile P",
	}})
	agent.RegisterHub(agent.Hub{Start: HubStart, Dispatch: NewHubDispatch,
		OwnPlan: func(id string) bool { return OpencodeCredID(id) == "opencode" }})
	agent.RegisterEnvRedactor("OPENCODE_CONFIG_CONTENT", RedactConfig)
	agent.Register(opencodeEngine{agent.NewBaseEngine("opencode")})
}
