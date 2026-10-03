// Package hooks implements `claude-code hook <name>`: JSON on stdin, exit 0 = proceed, 2 = block.
// Hooks fail OPEN — an infra glitch never blocks the session.
package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
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
	AgentID             string `json:"agent_id"`
	AgentTranscriptPath string `json:"agent_transcript_path"`

	Raw []byte
	doc map[string]any
}

// ParseInput never errors: malformed stdin yields a zero Input (fail-open).
func ParseInput(r io.Reader) *Input {
	raw, _ := io.ReadAll(r)
	in := &Input{Raw: raw}
	_ = json.Unmarshal(raw, in)
	return in
}

func (in *Input) nested(path ...string) string {
	if in.doc == nil {
		in.doc = map[string]any{}
		_ = json.Unmarshal(in.Raw, &in.doc)
	}
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

// ToolCommand is the Bash command under tool_input (legacy top-level .command tolerated).
func (in *Input) ToolCommand() string {
	if c := in.nested("tool_input", "command"); c != "" {
		return c
	}
	return in.nested("command")
}

// SubagentID names the subagent a SubagentStop belongs to: agent_id, else the id in agent-<id>.jsonl.
func (in *Input) SubagentID() string {
	if in.AgentID != "" {
		return in.AgentID
	}
	if id := in.nested("agentId"); id != "" {
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
		if s := in.nested(path...); s != "" {
			return s
		}
	}
	return ""
}

// contextCapChars stays under Claude Code's 10,000-char additionalContext ceiling (larger payloads are dropped).
const contextCapChars = 9500

// MergeContext joins context blocks into ONE additionalContext envelope; the protocol accepts a single object.
func MergeContext(event string, blocks []string) string {
	kept := blocks[:0:0]
	for _, b := range blocks {
		if b != "" {
			kept = append(kept, b)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	text := strings.Join(kept, "\n\n")
	if r := []rune(text); len(r) > contextCapChars {
		text = string(r[:contextCapChars]) + "…"
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": text}})
	return string(out)
}

// Handler runs one hook.
type Handler func(stdin io.Reader, stdout, stderr io.Writer) int

// Registry maps hook verbs to handlers; names must match hookspec.Registry.
var Registry = map[string]Handler{
	"safety":              func(in io.Reader, _, errw io.Writer) int { return Safety(in, errw) },
	"stop-format":         func(in io.Reader, _, _ io.Writer) int { return StopFormat(in) },
	"context-checkpoint":  func(in io.Reader, _, errw io.Writer) int { return ContextCheckpoint(in, errw) },
	"session-start":       func(in io.Reader, out, _ io.Writer) int { return SessionStart(in, out) },
	"memory-recall":       func(in io.Reader, out, _ io.Writer) int { return MemoryRecall(in, out) },
	"session-harvest":     func(in io.Reader, _, errw io.Writer) int { return SessionHarvest(in, errw) },
	"session-harvest-end": func(in io.Reader, _, _ io.Writer) int { return SessionHarvestEnd(in) },
	"agent-inflight":      func(in io.Reader, _, errw io.Writer) int { return AgentInflight("pre", in, errw) },
	"agent-inflight-tag":  func(in io.Reader, _, errw io.Writer) int { return AgentInflight("tag", in, errw) },
	"agent-inflight-post": func(in io.Reader, _, errw io.Writer) int { return AgentInflight("post", in, errw) },
	"save-plan":           func(in io.Reader, _, errw io.Writer) int { return SavePlan(in, errw) },
	"subagent-stop":       func(in io.Reader, _, errw io.Writer) int { return SubagentStop(in, errw) },
}

// Run dispatches a hook by name; an unknown name is a non-blocking error.
func Run(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	h, ok := Registry[name]
	if !ok {
		fmt.Fprintf(stderr, "claude-code hook: unknown hook %q\n", name)
		return 1
	}
	return h(stdin, stdout, stderr)
}
