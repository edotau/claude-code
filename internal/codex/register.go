package codex

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	agent.Register(agent.Agent{Row: row, Driver: codexEngine{agent.NewBaseEngine("codex")}})
	render.RegisterFileOps("codex", render.FileOps{Write: writeCodexConfig, LoadBaseline: LoadCodexBaseline,
		SaveBaseline: SaveCodexBaseline, Revert: restoreCodexConfig})
	render.RegisterFileOps("codex-hooks", render.FileOps{Write: writeCodexHooks})
}

// row is codex's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "codex", CLI: agent.CLI{
	PromptVia: agent.PromptStdin,
	Binary:    "codex", Dialects: []string{providers.ProtocolResponses},
	RouterSuffix: map[string]string{providers.ProtocolResponses: "/" + providers.ProtocolResponses},
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, Autonomous: agent.None, NoPersist: agent.Native, SafeMode: agent.Native, Direct: agent.Native, Env: agent.Native,
		SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, OutputSchema: agent.Native, Persona: agent.Emulated,
		Permissions: agent.Native},
	Events:       []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventToolResult, wire.EventResult, wire.EventError},
	ResumeFormat: "codex resume %s", ModelsProvider: "openai", LoginArgv: []string{"codex", "login"},
	ConfigureHint: "run `codex login` (ChatGPT) or `claude-code configure` → codex → API key",
	Extras:        map[string][]string{extraApprovalPolicy: nil, extraSandboxMode: nil, extraVerbosity: codexVerbosities},
}}

// The record extras codex reads: approval_policy and sandbox_mode (free-form) and model_verbosity in config.toml.
const (
	extraApprovalPolicy = "approvalPolicy"
	extraSandboxMode    = "sandboxMode"
	extraVerbosity      = "model_verbosity"
)

// codexVerbosities is the model_verbosity values codex accepts.
var codexVerbosities = []string{"low", "medium", "high"}
