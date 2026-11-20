package pi

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
)

func init() { agent.Register(agent.Agent{Row: row, Driver: piEngine{agent.NewBaseEngine("pi")}}) }

// row is pi's roster entry: it runs only on a routed Databricks profile, so it has no own login.
var row = agent.Schema{Name: "pi", CLI: agent.CLI{
	Flags: []string{"--mode", "--model", "--thinking", "--tools", "--no-tools", "--no-session", "--session", "--session-id",
		"--session-dir", "--system-prompt", "--append-system-prompt", "--no-context-files", "--no-mcp", "--approve"},
	HelpArgv: []string{"--help"},
	Binary:   "pi", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI},
	RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI,
		providers.ProtocolResponses: "/" + providers.ProtocolResponses},
	MaxVersion: "1.x", MaxReason: "2.x is untested with the harness pi route",
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, ReadOnly: agent.Native, Autonomous: agent.Native, NoPersist: agent.Native, Env: agent.Native,
		SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, WorkDir: agent.Emulated,
		Permissions: agent.None, SafeMode: agent.None, OutputSchema: agent.None, Persona: agent.None},
	Events:        []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventToolResult, wire.EventRetry, wire.EventResult, wire.EventError},
	PromptVia:     agent.PromptArgv,
	ConfigureHint: "run `claude-code install --tools pi`, then dispatch with --provider databricks",
}}
