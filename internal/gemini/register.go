package gemini

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

func init() {
	agent.Register(agent.Agent{Row: row, Driver: geminiEngine{agent.NewBaseEngine("gemini")}})
}

// row is gemini's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "gemini", CLI: agent.CLI{
	PromptVia: agent.PromptStdin,
	// agy print mode drops a single message's text past ~100–200 KB (probe 2026-11-05: found at 100 KB, lost at 200 KB)
	PromptMaxBytes: 100 * 1024,
	// agy print mode cannot carry a large message but reads a 300 KB file with tools (probe 2026-11-05, ZEBRA-290)
	PackVia: agent.PackFile,
	Binary:  "agy",
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, ReadOnly: agent.Emulated, Autonomous: agent.Native, DebugFile: agent.Native, Direct: agent.Native, Env: agent.Native,
		SystemPrompt: agent.Emulated, AppendSystemPrompt: agent.Emulated, OutputSchema: agent.Native, Persona: agent.Emulated,
		Permissions: agent.Native},
	Events: []string{wire.EventStart, wire.EventText, wire.EventProgress, wire.EventResult, wire.EventError}, Cost: true,
	ResumeFormat: "agy --conversation %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
	ConfigureHint: "sign in with `agy` (Google login)", DisplayName: "antigravity", LoginArgv: []string{"agy"},
	Extras: map[string][]string{extraUseGuardRules: {"true", "false"}},
}}

// extraUseGuardRules gates the guard denies in agy's settings and --settings; only "false" turns them off.
const extraUseGuardRules = "useGuardRules"
