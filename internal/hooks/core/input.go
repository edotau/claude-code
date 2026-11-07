// Package core holds the hook types every module shares: Input, Result, Runner, exit codes and agent renderers.
package core

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/settings"
)

// Exit codes per the Claude Code hook contract.
const (
	ExitProceed = 0
	ExitBlock   = 2
)

// Input is the parsed hook stdin; nested tool fields resolve through the raw document.
type Input struct {
	SessionID           string `json:"session_id"`
	CWD                 string `json:"cwd"`
	TranscriptPath      string `json:"transcript_path"`
	ToolName            string `json:"tool_name"`
	HookEventName       string `json:"hook_event_name"`
	StopHookActive      bool   `json:"stop_hook_active"`
	Prompt              string `json:"prompt"`
	Source              string `json:"source"`
	AgentID             string `json:"agent_id"`
	AgentTranscriptPath string `json:"agent_transcript_path"`
	Trigger             string `json:"trigger"`
	BackgroundTasks     int    `json:"-"` // length of the Stop payload's background_tasks array

	Raw   []byte `json:"-"`
	doc   map[string]any
	agent string
}

const (
	AgentClaude  = "claude"
	AgentCodex   = "codex"
	AgentCopilot = "copilot"
	AgentGemini  = "gemini" // agy
)

// Agent uses the explicit hook agent or Copilot's payload shape, defaulting to Claude.
func (in *Input) Agent() string {
	if in == nil || in.agent == "" {
		return AgentClaude
	}
	return in.agent
}

// ParseInput never errors: malformed stdin yields a zero Input (fail-open).
func ParseInput(r io.Reader) *Input {
	raw, _ := io.ReadAll(r)
	in := &Input{Raw: raw}
	if json.Unmarshal(raw, &in.doc) != nil {
		return &Input{Raw: raw}
	}
	switch name := os.Getenv(settings.AgentEnv); name {
	case AgentClaude, AgentCodex, AgentCopilot, AgentGemini:
		in.agent = name
	}
	in.SessionID = in.Nested("session_id")
	in.CWD = in.Nested("cwd")
	in.TranscriptPath = in.Nested("transcript_path")
	in.ToolName = in.Nested("tool_name")
	in.HookEventName = in.Nested("hook_event_name")
	in.StopHookActive, _ = in.doc["stop_hook_active"].(bool)
	in.Prompt = in.Nested("prompt")
	in.AgentID = in.Nested("agent_id")
	in.Source = in.Nested("source")
	in.AgentTranscriptPath = in.Nested("agent_transcript_path")
	in.Trigger = in.Nested("trigger")
	tasks, _ := in.doc["background_tasks"].([]any)
	in.BackgroundTasks = len(tasks)
	if _, ok := in.doc["sessionId"].(string); ok && in.HookEventName == "" {
		in.agent = AgentCopilot
	}
	if in.SessionID == "" {
		in.SessionID = in.Nested("sessionId")
	}
	if in.ToolName == "" {
		in.ToolName = in.Nested("toolName")
	}
	if in.CWD == "" {
		in.CWD = in.Nested("workingDirectory")
	}
	if in.TranscriptPath == "" {
		in.TranscriptPath = in.Nested("transcriptPath")
	}
	if in.Agent() == AgentCopilot {
		in.normalizeToolArgs()
	}
	if in.Agent() == AgentGemini {
		in.normalizeAgy()
	}
	return in
}

// normalizeAgy reads agy's {toolCall:{name,args}, workspacePaths} payload as tool_name/tool_input/cwd.
func (in *Input) normalizeAgy() {
	call, _ := in.doc["toolCall"].(map[string]any)
	args, _ := call["args"].(map[string]any)
	input := maps.Clone(args)
	if input == nil {
		input = map[string]any{}
	}
	if cmd, _ := args["CommandLine"].(string); cmd != "" {
		input["command"] = cmd
	}
	if path, _ := args["Path"].(string); path != "" {
		input["file_path"] = path
	}
	in.ToolName, _ = call["name"].(string)
	in.doc["tool_input"] = input
	if paths, _ := in.doc["workspacePaths"].([]any); len(paths) > 0 && in.CWD == "" {
		in.CWD, _ = paths[0].(string)
	}
}

// normalizeToolArgs lets every existing tool_input consumer read Copilot's string or object arguments.
func (in *Input) normalizeToolArgs() {
	args, _ := in.doc["toolArgs"].(map[string]any)
	if text, ok := in.doc["toolArgs"].(string); ok {
		_ = json.Unmarshal([]byte(text), &args)
	}
	input, _ := in.doc["tool_input"].(map[string]any)
	if input == nil {
		input = args
	}
	if input == nil {
		return
	}
	for _, key := range []string{"file_path", "filePath", "path"} {
		if path, _ := input[key].(string); path != "" {
			if !filepath.IsAbs(path) && in.CWD != "" {
				path = filepath.Join(in.CWD, path)
			}
			input["file_path"] = path
			break
		}
	}
	in.doc["tool_input"] = input
}

// Field is a top-level key of the raw hook document; nil when absent.
func (in *Input) Field(key string) any { return in.doc[key] }

func (in *Input) Nested(path ...string) string {
	cur := any(in.doc)
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[k]
	}
	s, _ := cur.(string)
	return s
}

// ToolInput returns the decoded tool arguments, or nil when absent.
func (in *Input) ToolInput() map[string]any {
	input, _ := in.doc["tool_input"].(map[string]any)
	return input
}

// CloneToolInput preserves arguments while allowing a hook to replace individual keys.
func (in *Input) CloneToolInput() map[string]any {
	input := in.ToolInput()
	if input == nil {
		return nil
	}
	clone := make(map[string]any, len(input)+1)
	maps.Copy(clone, input)
	return clone
}

// ToolCommand is the Bash command under tool_input (a top-level .command is the fallback).
func (in *Input) ToolCommand() string {
	if c := in.Nested("tool_input", "command"); c != "" {
		return c
	}
	return in.Nested("command")
}

// SubagentID names the subagent a SubagentStop belongs to: agent_id, else the id in agent-<id>.jsonl.
func (in *Input) SubagentID() string {
	if in.AgentID != "" {
		return in.AgentID
	}
	if id := in.Nested("agentId"); id != "" {
		return id
	}
	base := strings.TrimSuffix(filepath.Base(in.AgentTranscriptPath), ".jsonl")
	for _, p := range []string{"agent-", "agent_"} {
		if rest, ok := strings.CutPrefix(base, p); ok {
			return strings.TrimSuffix(rest, "_transcript")
		}
	}
	return ""
}

// ToolResponseText is tool_response as text: a bare string for most tools, an object with a text leaf for some.
func (in *Input) ToolResponseText() string {
	for _, path := range [][]string{{"tool_response"}, {"tool_response", "output"}, {"tool_response", "content"}} {
		if s := in.Nested(path...); s != "" {
			return s
		}
	}
	return ""
}

// ContextCapChars stays under Claude Code's 10,000-char additionalContext ceiling (larger payloads are dropped).
const ContextCapChars = 9500

// HookContext gives subprocesses the registered hook budget without changing process signal handling.
func HookContext(name string) (context.Context, context.CancelFunc) {
	for _, spec := range settings.Specs() {
		if spec.Name == name {
			return context.WithTimeout(context.Background(), spec.Budget)
		}
	}
	panic("unknown hook: " + name)
}

// SpawnTools are Claude Code's subagent-spawn tool names (Agent; Task on older builds).
var SpawnTools = map[string]bool{"Agent": true, "Task": true}
