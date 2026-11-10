package memhook

import (
	"os"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/settings"
)

// harnessAgentKey is the payload field the in-process opencode host sets, where no HARNESS_HOOK_AGENT env exists.
const harnessAgentKey = "harness_agent"

// AgentInput returns in with TranscriptPath pointing at a Claude-shaped transcript synthesized from an opencode
// export or a Copilot events.jsonl; any other input, or a failed load, comes back unchanged (fail open).
func AgentInput(in *core.Input) *core.Input {
	kind := agentKind(in)
	if kind == "" {
		return in
	}
	path, ok := memory.SynthAgentTranscript(kind, in.SessionID, in.CWD, in.TranscriptPath)
	if !ok {
		return in
	}
	out := *in
	out.TranscriptPath = path
	return &out
}

// agentKind names the memory source whose transcript AgentInput synthesizes, or "" for a Claude-shaped input.
func agentKind(in *core.Input) string {
	switch {
	case os.Getenv(settings.AgentEnv) == memory.SourceOpencode || in.Nested(harnessAgentKey) == memory.SourceOpencode:
		return memory.SourceOpencode
	case in.Agent() == core.AgentCopilot && in.TranscriptPath != "":
		return memory.SourceCopilot
	}
	return ""
}
