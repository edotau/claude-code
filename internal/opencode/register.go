package opencode

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	agent.Register(agent.Agent{Row: row, Driver: opencodeEngine{agent.NewBaseEngine("opencode")},
		Hub: &agent.Hub{Start: HubStart, Dispatch: NewHubDispatch,
			OwnPlan: func(id string) bool { return OpencodeCredID(id) == "opencode" }},
		Redactors: map[string]func(string) string{"OPENCODE_CONFIG_CONTENT": RedactConfig}})
}

// row is opencode's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "opencode", CLI: agent.CLI{
	Binary: "opencode", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolGemini, providers.ProtocolOpenAI},
	RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolGemini: "/" + providers.ProtocolGemini,
		providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI, providers.ProtocolResponses: "/" + providers.ProtocolResponses},
	MaxVersion: "1.x", MaxReason: "2.x broke the harness opencode route", // keep in step with opencodeMaxMajor in internal/agent/tools/install.go
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, Autonomous: agent.Native, SafeMode: agent.Native, Direct: agent.Native, Env: agent.Native,
		SystemPrompt: agent.Native, Permissions: agent.Native, AppendSystemPrompt: agent.Emulated, OutputSchema: agent.Emulated, Persona: agent.Emulated},
	Events:       []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventToolResult, wire.EventRetry, wire.EventResult, wire.EventError},
	ResumeFormat: "opencode run --session %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
	PromptVia: agent.PromptArgv, OwnLoginProfile: OwnLogin, PinIDs: providers.RouteOpencodeSubIDs, DirFlag: true, Hub: true, LoginArgv: []string{"opencode", "auth", "login"},
	ConfigureHint: "run `opencode auth login` (opencode Go) or `claude-code configure github` (Copilot)",
	CatalogUsage:  "model menu: list|add|remove|import|reset|check|sync",
	CatalogHint:   "Serving-only discovery: claude-code opencode catalog sync --profile P",
}}
