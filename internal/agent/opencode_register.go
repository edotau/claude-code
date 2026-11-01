package agent

import (
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	RegisterRow(3, Schema{Name: "opencode", CLI: CLI{
		Binary: "opencode", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolGemini, providers.ProtocolOpenAI},
		RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolGemini: "/" + providers.ProtocolGemini,
			providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI, providers.ProtocolResponses: "/" + providers.ProtocolResponses},
		MaxVersion: "1.x", MaxReason: "2.x broke the harness opencode route", // keep in step with opencodeMaxMajor
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, Autonomous: Native, SafeMode: Native, Direct: Native, Env: Native,
			SystemPrompt: Native, AppendSystemPrompt: Emulated, OutputSchema: Emulated, Persona: Emulated},
		Events:       []string{EventStart, EventText, EventToolUse, EventToolResult, EventRetry, EventResult, EventError},
		ResumeFormat: "opencode run --session %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
		PromptInArgv: true, DirFlag: true, Hub: true, LoginArgv: []string{"opencode", "auth", "login"},
		ConfigureHint: "run `opencode auth login` (opencode Go) or `claude-code configure github` (Copilot)",
		CatalogUsage:  "model menu: list|add|remove|import|reset|check|sync",
		CatalogHint:   "Serving-only discovery: claude-code opencode catalog sync --profile P",
	}})
	Register(opencodeEngine{BaseEngine{"opencode"}})
}
