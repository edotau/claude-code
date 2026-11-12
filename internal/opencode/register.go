package opencode

import (
	"fmt"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	agent.Register(agent.Agent{Row: row, Driver: opencodeEngine{agent.NewBaseEngine("opencode")},
		Hub: &agent.Hub{Start: HubStart, Dispatch: newHubDispatch,
			OwnPlan: func(id string) bool { return OpencodeCredID(id) == "opencode" }},
		Redactors: map[string]func(string) string{"OPENCODE_CONFIG_CONTENT": redactConfig}})
}

// row is opencode's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "opencode", CLI: agent.CLI{
	Binary: "opencode", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolGemini, providers.ProtocolOpenAI},
	RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolGemini: "/" + providers.ProtocolGemini,
		providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI, providers.ProtocolResponses: "/" + providers.ProtocolResponses},
	MaxVersion: "1.x", MaxReason: "2.x broke the harness opencode route", // keep in step with opencodeMaxMajor in internal/agent/tools/install.go
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, ReadOnly: agent.Emulated, Autonomous: agent.Native, SafeMode: agent.Native, Direct: agent.Native, Env: agent.Native,
		SystemPrompt: agent.Native, Permissions: agent.Native, AppendSystemPrompt: agent.Emulated, OutputSchema: agent.Emulated, Persona: agent.Emulated},
	Events:       []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventToolResult, wire.EventRetry, wire.EventResult, wire.EventError},
	ResumeFormat: "opencode run --session %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
	PromptVia: agent.PromptArgv, OwnLoginProfile: OwnLogin, PinIDs: providers.RouteOpencodeSubIDs, DirFlag: true, Hub: true, LoginArgv: []string{"opencode", "auth", "login"},
	ConfigureHint: "run `opencode auth login` (opencode Go) or `claude-code configure github` (Copilot)",
	CatalogUsage:  "model menu: list|add|remove|import|reset|check|sync",
	CatalogHint:   "Serving-only discovery: claude-code opencode catalog sync --profile P",
}}

// describeOpenCodeRoute: 1) opencode's own login (Go plan); 2) opencode's GitHub Copilot login; 3) none.
func describeOpenCodeRoute(reg *providers.Registry) agent.RouteInfo {
	if OpencodeOwnLoginPresent() {
		return agent.RouteInfo{Agent: "opencode", Route: agent.RouteOwnLogin, Provider: "opencode-go",
			Reason: "opencode's own opencode login (Go plan; OPENCODE_API_KEY from the vault when no stored login)", OK: true}
	}
	if OpencodeCopilotLoginPresent() {
		if p, ok := reg.LoginProfile("github", "copilot"); ok {
			if main, _, mok := opencodeCopilotModelIDs(reg); mok {
				return agent.RouteInfo{Agent: "opencode", Route: agent.RouteCopilotLogin, Provider: p.Name, Model: main,
					Reason: "opencode's own GitHub Copilot login (opencode auth login)", OK: true}
			}
			return agent.RouteInfo{Agent: "opencode", Route: agent.RouteCopilotLogin, Provider: p.Name,
				Reason: "opencode's own GitHub Copilot login, but no sonnet/haiku model configured for github-copilot"}
		}
	}
	s, _ := agent.LookupSchema("opencode")
	return agent.RouteInfo{Agent: "opencode", Route: agent.RouteNone, Reason: "no route for opencode: " + s.CLI.ConfigureHint}
}

// opencodeRouteDetail reports the opencode route `claude-code configure --restore` would render: provider,
// model/small_model (the github-copilot route only), the file it writes, and — names/booleans only,
// never a credential — whether opencode has its own GitHub Copilot login and whether the copilot CLI has one.
func opencodeRouteDetail(reg *providers.Registry, sel providers.Selection, selErr error) string {
	own := OpencodeOwnLoginPresent()
	opencodeLogin := own || OpencodeCopilotLoginPresent()
	copilotCLILogin := agent.SchemaFor("copilot").LoginPresent()
	pin, err := agent.PinnedProfile("opencode") // a pinned profile is what a launch uses, as cli.go does
	switch {
	case err != nil:
		selErr = err
	case pin != "":
		sel, selErr = providers.Select(reg, pin)
	case own: // opencode's own Go/Zen credential serves the routed opencode dispatches, whatever the session provider
		return fmt.Sprintf("provider=opencode-go dispatch=%s opencode_login=true copilot_cli_login=%v", agent.RouteOwnLogin, copilotCLILogin)
	}
	if selErr != nil {
		return fmt.Sprintf("opencode_login=%v copilot_cli_login=%v (%v)", opencodeLogin, copilotCLILogin, selErr)
	}
	target, _, err := agent.SchemaFor("opencode").ResolveTarget(reg, sel, pin != "")
	if err != nil || target.Provider == nil {
		return fmt.Sprintf("opencode_login=%v copilot_cli_login=%v (%v)", opencodeLogin, copilotCLILogin, errText(err))
	}
	file := filepath.Join(paths.StateDir(), "opencode", "opencode.json") // the overlay, not opencode's own config
	if target.Provider.Provider != "github" {
		return fmt.Sprintf("provider=%s file=%s opencode_login=%v copilot_cli_login=%v", target.Provider.Name, file, opencodeLogin, copilotCLILogin)
	}
	mainID, smallID, ok := opencodeCopilotModelIDs(reg)
	if !ok {
		return fmt.Sprintf("provider=%s file=%s (no sonnet/haiku model configured) opencode_login=%v copilot_cli_login=%v", target.Provider.Name, file, opencodeLogin, copilotCLILogin)
	}
	// opus/sonnet/haiku here are github-copilot's own table (configure github), separate from COPILOT_MODEL.
	opusID := target.Provider.Models[models.Opus]
	return fmt.Sprintf("provider=%s model=github-copilot/%s small_model=github-copilot/%s opus/sonnet/haiku=%s/%s/%s file=%s opencode_login=%v copilot_cli_login=%v",
		target.Provider.Name, mainID, smallID, opusID, mainID, smallID, file, opencodeLogin, copilotCLILogin)
}

// errText is err's message, or "" for nil.
func errText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
