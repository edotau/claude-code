// Package core holds the hook types every module shares: Input, Result, Runner, exit codes and agent renderers.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
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

	Raw     []byte `json:"-"`
	Invalid string `json:"-"` // why a non-empty payload could not be read or parsed; "" when it parsed
	doc     map[string]any
	agent   string
}

const (
	AgentClaude   = agentid.Claude
	AgentCodex    = agentid.Codex
	AgentCopilot  = agentid.Copilot
	AgentGemini   = agentid.Gemini // agy
	AgentOpencode = agentid.OpenCode
)

// harnessAgentKey is the payload field the in-process opencode host sets, where no HARNESS_HOOK_AGENT env exists.
const harnessAgentKey = "harness_agent"

// Agent uses the explicit hook agent, opencode's payload key or Copilot's payload shape, defaulting to Claude.
func (in *Input) Agent() string {
	if in == nil || in.agent == "" {
		return AgentClaude
	}
	return in.agent
}

// ParseInput never errors: malformed stdin yields a zero Input with Invalid set, empty stdin a zero Input.
func ParseInput(r io.Reader) *Input { return ParseInputFor(r, os.Getenv(settings.AgentEnv)) }

// ParseInputFor parses as ParseInput with the hook agent given explicitly ("" = detect from the payload).
func ParseInputFor(r io.Reader, agent string) *Input {
	raw, err := io.ReadAll(r)
	if err != nil {
		return &Input{Raw: raw, Invalid: "read: " + err.Error(), agent: knownAgent(agent)}
	}
	in := &Input{Raw: raw}
	if err := json.Unmarshal(raw, &in.doc); err != nil {
		return unparsed(raw, agent, err)
	}
	if in.doc == nil { // stdin `null` unmarshals without error and leaves the map nil
		return unparsed(raw, agent, errNotObject)
	}
	in.agent = knownAgent(agent)
	if in.Nested(harnessAgentKey) == AgentOpencode { // only opencode's own host writes the key, so it wins over the env
		in.agent = AgentOpencode
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
	in.Invalid = in.normalizeTool()
	return in
}

var errNotObject = errors.New("payload is not a JSON object")

// normalizeTool maps the host's tool arguments onto tool_input; non-empty is why present arguments are misshapen.
func (in *Input) normalizeTool() string {
	switch in.Agent() {
	case AgentCopilot:
		return in.normalizeToolArgs()
	case AgentGemini:
		return in.normalizeAgy()
	}
	return shapeError(in.doc, "tool_input")
}

// shapeError names field when doc holds it as something other than an object or null.
func shapeError(doc map[string]any, field string) string {
	switch doc[field].(type) {
	case nil, map[string]any:
		return ""
	}
	return field + ": not a JSON object"
}

// copilotArgs decodes toolArgs (an object, or a string holding one); blank, absent or null is no arguments.
func copilotArgs(v any) (map[string]any, string) {
	switch v := v.(type) {
	case nil:
		return nil, ""
	case map[string]any:
		return v, ""
	case string:
		var args map[string]any
		if strings.TrimSpace(v) == "" {
			return nil, ""
		}
		if err := json.Unmarshal([]byte(v), &args); err != nil {
			return nil, "toolArgs: " + err.Error()
		}
		return args, ""
	}
	return nil, "toolArgs: not a JSON object"
}

// knownAgent is agent when it names a supported hook agent, else "" (detect from the payload).
func knownAgent(agent string) string {
	switch agent {
	case AgentClaude, AgentCodex, AgentCopilot, AgentGemini, AgentOpencode:
		return agent
	}
	return ""
}

// unparsed is the zero Input for stdin that is not a JSON object; empty stdin stays valid, as Stop may carry none.
func unparsed(raw []byte, agent string, err error) *Input {
	if len(bytes.TrimSpace(raw)) == 0 {
		return &Input{Raw: raw}
	}
	return &Input{Raw: raw, Invalid: err.Error(), agent: knownAgent(agent)}
}

// normalizeAgy reads agy's {toolCall:{name,args}, workspacePaths} payload as tool_name/tool_input/cwd.
func (in *Input) normalizeAgy() string {
	if bad := shapeError(in.doc, "toolCall"); bad != "" {
		return bad
	}
	call, _ := in.doc["toolCall"].(map[string]any)
	if bad := shapeError(call, "args"); bad != "" {
		return "toolCall." + bad
	}
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
	return ""
}

// normalizeToolArgs lets every existing tool_input consumer read Copilot's string or object arguments.
func (in *Input) normalizeToolArgs() string {
	args, bad := copilotArgs(in.doc["toolArgs"])
	if bad == "" {
		bad = shapeError(in.doc, "tool_input")
	}
	if bad != "" {
		return bad
	}
	input, _ := in.doc["tool_input"].(map[string]any)
	if input == nil {
		input = args
	}
	if input == nil {
		return ""
	}
	for _, key := range []string{"file_path", "filePath", "path"} {
		if path, _ := input[key].(string); path != "" {
			input["file_path"] = AnchorPath(path, in.CWD)
			break
		}
	}
	in.doc["tool_input"] = input
	return ""
}

// toolAliases maps an agent's tool names to Claude's; renderers map back (CopilotJSON, agyGateTools, codexMatcher).
var toolAliases = map[string]map[string]string{
	AgentCodex:   {"apply_patch": "Edit"},
	AgentCopilot: {"create": "Write", "view": "Read"},
	AgentGemini:  {"run_command": "Bash", "write_file": "Write", "edit_file": "Edit", "read_file": "Read"},
}

// CanonicalTool is ToolName under Claude's name, the one hookspec matchers select on; handlers still read ToolName.
func (in *Input) CanonicalTool() string {
	if name, ok := toolAliases[in.Agent()][in.ToolName]; ok {
		return name
	}
	return in.ToolName
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

// specsByName indexes the static hookspec registry once per process, keeping the first entry per name.
var specsByName = sync.OnceValue(func() map[string]settings.EventSpec {
	m := map[string]settings.EventSpec{}
	for _, s := range settings.Specs() {
		if _, dup := m[s.Name]; !dup {
			m[s.Name] = s
		}
	}
	return m
})

// SpecOf is the hookspec entry for a module, stamped with its group; false when unknown.
func SpecOf(name string) (settings.EventSpec, bool) {
	s, ok := specsByName()[name]
	return s, ok
}

// HookMargin is how long before the dispatcher's budget timer a handler's own deadline expires; a test seam.
var HookMargin = time.Second

// HookContext gives subprocesses the budget less HookMargin (or half it), so a handler fails open before an overrun.
func HookContext(name string) (context.Context, context.CancelFunc) {
	spec, ok := SpecOf(name)
	if !ok {
		panic("unknown hook: " + name)
	}
	budget := spec.Budget - HookMargin
	if budget <= 0 {
		budget = spec.Budget / 2
	}
	return context.WithTimeout(context.Background(), budget)
}

// SpawnTools are Claude Code's subagent-spawn tool names (Agent; Task on older builds).
var SpawnTools = map[string]bool{"Agent": true, "Task": true}
