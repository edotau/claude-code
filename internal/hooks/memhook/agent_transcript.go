package memhook

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// AgentTranscript is what memory-update/memory-end/format-stop need from a non-Claude session.
type AgentTranscript struct {
	ID          string
	Edits       int
	EditedPaths []string // unique, sorted
	FinalText   string
}

var opencodeEditTools = map[string]bool{"write": true, "edit": true, "patch": true, "multiedit": true}

// copilotEditTools are Copilot CLI editor tool names; unverified against a live event log.
var copilotEditTools = map[string]bool{"edit": true, "create": true, "write": true, "str_replace_editor": true, "apply_patch": true}

// pathBuilder dedups edited paths while counting every edit call.
type pathBuilder struct {
	seen map[string]bool
	out  AgentTranscript
}

func (b *pathBuilder) edit(path string) {
	b.out.Edits++
	if path != "" && !b.seen[path] {
		b.seen[path] = true
		b.out.EditedPaths = append(b.out.EditedPaths, path)
	}
}

func (b *pathBuilder) done() AgentTranscript {
	sort.Strings(b.out.EditedPaths)
	return b.out
}

// LoadOpencodeExport parses `opencode export <id>` JSON; final text is the last assistant text part.
func LoadOpencodeExport(data []byte) (AgentTranscript, error) {
	var doc struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
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
		return AgentTranscript{}, fmt.Errorf("opencode export: %w", err)
	}
	b := &pathBuilder{seen: map[string]bool{}}
	b.out.ID = doc.Info.ID
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

// CopilotEvent is the slice of an SDK session event the loader reads; the engine fills it from rpc.SessionEvent.
type CopilotEvent struct {
	Type      string // "tool.execution_start" or "assistant.message"
	ToolName  string
	Arguments map[string]any
	Content   string
	AgentID   string // non-empty for sub-agent events, which are skipped
}

// LoadCopilotEvents folds an in-memory Copilot event list; copilot has no on-disk transcript.
func LoadCopilotEvents(sessionID string, events []CopilotEvent) AgentTranscript {
	b := &pathBuilder{seen: map[string]bool{}}
	b.out.ID = sessionID
	for _, e := range events {
		if e.AgentID != "" {
			continue
		}
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
