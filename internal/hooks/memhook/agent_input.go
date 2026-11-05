package memhook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/transcript"
)

// harnessAgentKey is the payload field the in-process opencode host sets, where no HARNESS_HOOK_AGENT env exists.
const harnessAgentKey = "harness_agent"

const (
	agentOpencode  = "opencode"
	exportTimeout  = 15 * time.Second
	exportMaxBytes = 64 << 20
)

// opencodeExport runs `opencode export <id>` in cwd; a seam so tests feed recorded bytes.
var opencodeExport = func(sessionID, cwd string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "opencode", "export", sessionID)
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if out.Len() > exportMaxBytes {
		return nil, os.ErrInvalid
	}
	return out.Bytes(), nil
}

// agentInput returns in with TranscriptPath pointing at a Claude-shaped transcript synthesized from an opencode
// export or a Copilot events.jsonl; any other input, or a failed load, comes back unchanged (fail open).
func agentInput(in *core.Input) *core.Input {
	var at AgentTranscript
	var err error
	switch {
	case in.SessionID == "":
		return in
	case os.Getenv(settings.AgentEnv) == agentOpencode || in.Nested(harnessAgentKey) == agentOpencode:
		var data []byte
		if data, err = opencodeExport(in.SessionID, in.CWD); err == nil {
			at, err = LoadOpencodeExport(data)
		}
	case in.Agent() == core.AgentCopilot && in.TranscriptPath != "":
		at = LoadCopilotEvents(in.SessionID, copilotEventsFrom(in.TranscriptPath, in.CWD))
	default:
		return in
	}
	if err != nil {
		return in
	}
	path, err := writeSynthTranscript(in.SessionID, at)
	if err != nil {
		return in
	}
	out := *in
	out.TranscriptPath = path
	return &out
}

// writeSynthTranscript writes one Claude-shaped Edit line per edit plus the final text; the path is stable per session.
func writeSynthTranscript(sessionID string, at AgentTranscript) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := 0; i < at.Edits; i++ {
		p := ""
		if len(at.EditedPaths) > 0 {
			p = at.EditedPaths[i%len(at.EditedPaths)]
		}
		_ = enc.Encode(synthLine("tool_use", map[string]any{"name": "Edit", "input": map[string]any{"file_path": p}}))
	}
	if at.FinalText != "" {
		_ = enc.Encode(synthLine("text", map[string]any{"text": at.FinalText}))
	}
	dest := filepath.Join(memstore.StateDir(), "agent-transcripts", filepath.Base(sessionID)+".jsonl")
	_, err := paths.WriteIfChanged(dest, buf.Bytes(), 0o600)
	return dest, err
}

func synthLine(kind string, block map[string]any) map[string]any {
	block["type"] = kind
	return map[string]any{"type": "assistant", "message": map[string]any{"content": []any{block}}}
}

var patchFileRe = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$`)

// copilotEventsFrom reads a Copilot events.jsonl into the loader's events; apply_patch text expands to one event per file.
func copilotEventsFrom(path, cwd string) []CopilotEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var evs []CopilotEvent
	_ = transcript.EachLine(f, func(line []byte) {
		var rec struct {
			Type string `json:"type"`
			Data struct {
				ToolName  string          `json:"toolName"`
				Arguments json.RawMessage `json:"arguments"`
				Content   string          `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return
		}
		switch rec.Type {
		case "assistant.message":
			evs = append(evs, CopilotEvent{Type: rec.Type, Content: rec.Data.Content})
		case "tool.execution_start":
			evs = append(evs, copilotToolEvents(rec.Data.ToolName, rec.Data.Arguments, cwd)...)
		}
	})
	return evs
}

func copilotToolEvents(tool string, raw json.RawMessage, cwd string) []CopilotEvent {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var args map[string]any
		_ = json.Unmarshal(raw, &args)
		return []CopilotEvent{{Type: "tool.execution_start", ToolName: tool, Arguments: args}}
	}
	var out []CopilotEvent
	for _, m := range patchFileRe.FindAllStringSubmatch(text, -1) {
		p := strings.TrimSpace(m[1])
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		out = append(out, CopilotEvent{Type: "tool.execution_start", ToolName: tool, Arguments: map[string]any{"path": p}})
	}
	return out
}
