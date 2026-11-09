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
	cmd.Stdout = &capWriter{buf: &out, max: exportMaxBytes}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// capWriter fails a write that would grow buf past max, so the child is cut off before memory grows unbounded.
type capWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		return 0, os.ErrInvalid
	}
	return w.buf.Write(p)
}

// AgentInput returns in with TranscriptPath pointing at a Claude-shaped transcript synthesized from an opencode
// export or a Copilot events.jsonl; any other input, or a failed load, comes back unchanged (fail open).
func AgentInput(in *core.Input) *core.Input {
	var at agentTranscript
	var err error
	switch {
	case in.SessionID == "":
		return in
	case os.Getenv(settings.AgentEnv) == agentOpencode || in.Nested(harnessAgentKey) == agentOpencode:
		var data []byte
		if data, err = opencodeExport(in.SessionID, in.CWD); err == nil {
			at, err = loadOpencodeExport(data)
		}
	case in.Agent() == core.AgentCopilot && in.TranscriptPath != "":
		// copilotEventsFrom turns an open error into no events; check the open here so it fails open instead.
		if err = canOpen(in.TranscriptPath); err == nil {
			at = loadCopilotEvents(in.SessionID, copilotEventsFrom(in.TranscriptPath, in.CWD))
		}
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

// canOpen reports whether path opens for reading.
func canOpen(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// writeSynthTranscript writes one Claude-shaped Edit line per edit plus the final text; the path is stable per session.
func writeSynthTranscript(sessionID string, at agentTranscript) (string, error) {
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

// copilotRec is the part of one events.jsonl line the loader reads.
type copilotRec struct {
	Type string `json:"type"`
	Data struct {
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		Arguments  json.RawMessage `json:"arguments"`
		Content    string          `json:"content"`
		Success    bool            `json:"success"`
		FileEdits  []struct {
			Path string `json:"path"`
		} `json:"fileEdits"`
	} `json:"data"`
}

// copilotStart is one tool.execution_start with its call id, so a failed completion can drop it.
type copilotStart struct {
	id  string
	evs []copilotEvent
}

// copilotEventsFrom reads a Copilot events.jsonl into the loader's events. Successful tool.execution_complete
// fileEdits are the edit source; start arguments (apply_patch text expanded per file) only when no fileEdits exist,
// and never for a call whose completion reported success:false.
func copilotEventsFrom(path, cwd string) []copilotEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var texts, done []copilotEvent
	var starts []copilotStart
	failed := map[string]bool{}
	var sawFileEdits bool
	_ = transcript.EachLine(f, func(line []byte) {
		var rec copilotRec
		if json.Unmarshal(line, &rec) != nil {
			return
		}
		switch rec.Type {
		case "assistant.message":
			texts = append(texts, copilotEvent{Type: rec.Type, Content: rec.Data.Content})
		case "tool.execution_start":
			starts = append(starts, copilotStart{id: rec.Data.ToolCallID, evs: copilotToolEvents(rec.Data.ToolName, rec.Data.Arguments, cwd)})
		case "tool.execution_complete":
			sawFileEdits = sawFileEdits || len(rec.Data.FileEdits) > 0
			if !rec.Data.Success && rec.Data.ToolCallID != "" {
				failed[rec.Data.ToolCallID] = true
			}
			for _, fe := range rec.Data.FileEdits {
				if rec.Data.Success {
					done = append(done, copilotEvent{Type: "tool.execution_start", ToolName: "edit", Arguments: map[string]any{"path": fe.Path}})
				}
			}
		}
	})
	if sawFileEdits {
		return append(texts, done...)
	}
	for _, s := range starts {
		if !failed[s.id] {
			texts = append(texts, s.evs...)
		}
	}
	return texts
}

func copilotToolEvents(tool string, raw json.RawMessage, cwd string) []copilotEvent {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var args map[string]any
		_ = json.Unmarshal(raw, &args)
		return []copilotEvent{{Type: "tool.execution_start", ToolName: tool, Arguments: args}}
	}
	var out []copilotEvent
	for _, m := range patchFileRe.FindAllStringSubmatch(text, -1) {
		p := strings.TrimSpace(m[1])
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		out = append(out, copilotEvent{Type: "tool.execution_start", ToolName: tool, Arguments: map[string]any{"path": p}})
	}
	return out
}
