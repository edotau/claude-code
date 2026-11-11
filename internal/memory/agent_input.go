package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// Agent sources whose sessions have no Claude-shaped transcript of their own.
const (
	SourceOpencode = "opencode"
	SourceCopilot  = "copilot"
)

const (
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

// SynthAgentTranscript writes a Claude-shaped transcript for an opencode or copilot session; ok=false fails open.
func SynthAgentTranscript(source, sessionID, cwd, transcriptPath string) (path string, ok bool) {
	var at agentTranscript
	var err error
	switch {
	case sessionID == "":
		return "", false
	case source == SourceOpencode:
		var data []byte
		if data, err = opencodeExport(sessionID, cwd); err == nil {
			at, err = loadOpencodeExport(data)
		}
	case source == SourceCopilot:
		// copilotEventsFrom turns an open error into no events; check the open here so it fails open instead.
		if err = canOpen(transcriptPath); err == nil {
			at = loadCopilotEvents(copilotEventsFrom(transcriptPath, cwd))
		}
	default:
		return "", false
	}
	if err != nil {
		return "", false
	}
	path, err = writeSynthTranscript(sessionID, at)
	if err != nil {
		return "", false
	}
	return path, true
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
	dest := filepath.Join(StateDir(), "agent-transcripts", filepath.Base(sessionID)+".jsonl")
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
			if !rec.Data.Success {
				if rec.Data.ToolCallID != "" {
					failed[rec.Data.ToolCallID] = true
				}
				return
			}
			sawFileEdits = sawFileEdits || len(rec.Data.FileEdits) > 0
			for _, fe := range rec.Data.FileEdits {
				done = append(done, copilotEvent{Type: "tool.execution_start", ToolName: "edit", Arguments: map[string]any{"path": fe.Path}})
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

// agentTranscript is what memory-update/memory-end/format-stop need from a non-Claude session.
type agentTranscript struct {
	Edits       int
	EditedPaths []string // unique, sorted
	FinalText   string
}

var opencodeEditTools = map[string]bool{"write": true, "edit": true, "patch": true, "multiedit": true}

// copilotEditTools are Copilot CLI editor tool names; create and edit verified live (1.0.105), the rest are guesses.
var copilotEditTools = map[string]bool{"edit": true, "create": true, "write": true, "str_replace_editor": true, "apply_patch": true}

// pathBuilder dedups edited paths while counting every edit call.
type pathBuilder struct {
	seen map[string]bool
	out  agentTranscript
}

func (b *pathBuilder) edit(path string) {
	b.out.Edits++
	if path != "" && !b.seen[path] {
		b.seen[path] = true
		b.out.EditedPaths = append(b.out.EditedPaths, path)
	}
}

func (b *pathBuilder) done() agentTranscript {
	sort.Strings(b.out.EditedPaths)
	return b.out
}

// loadOpencodeExport parses `opencode export <id>` JSON; final text is the last assistant text part.
func loadOpencodeExport(data []byte) (agentTranscript, error) {
	var doc struct {
		Messages []struct {
			Info struct {
				Role string `json:"role"`
			} `json:"info"`
			Parts []struct {
				Type  string `json:"type"`
				Tool  string `json:"tool"`
				Text  string `json:"text"`
				State struct {
					Status string `json:"status"`
					Input  struct {
						FilePath string `json:"filePath"`
					} `json:"input"`
				} `json:"state"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return agentTranscript{}, fmt.Errorf("opencode export: %w", err)
	}
	b := &pathBuilder{seen: map[string]bool{}}
	for _, m := range doc.Messages {
		if m.Info.Role != "assistant" {
			continue
		}
		for _, p := range m.Parts {
			switch {
			case p.Type == "tool" && opencodeEditTools[strings.ToLower(p.Tool)] && p.State.Status == "completed":
				b.edit(p.State.Input.FilePath)
			case p.Type == "text" && p.Text != "":
				b.out.FinalText = p.Text
			}
		}
	}
	return b.done(), nil
}

// copilotEvent is the normalized event read from a Copilot events.jsonl.
type copilotEvent struct {
	Type      string // "tool.execution_start" or "assistant.message"
	ToolName  string
	Arguments map[string]any
	Content   string
}

// loadCopilotEvents folds normalized Copilot events into transcript data.
func loadCopilotEvents(events []copilotEvent) agentTranscript {
	b := &pathBuilder{seen: map[string]bool{}}
	for _, e := range events {
		switch {
		case e.Type == "tool.execution_start" && copilotEditTools[strings.ToLower(e.ToolName)]:
			b.edit(copilotArgPath(e.Arguments))
		case e.Type == "assistant.message" && e.Content != "":
			b.out.FinalText = e.Content
		}
	}
	return b.done()
}

func copilotArgPath(args map[string]any) string {
	for _, k := range []string{"path", "filePath", "file_path"} {
		if s, ok := args[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
