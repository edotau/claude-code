// Package hooks implements `claude-code hook <name>`: JSON on stdin, exit 0 = proceed, 2 = block.
// Hooks fail OPEN — an infra glitch never blocks the session.
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hookspec"
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

	Raw    []byte `json:"-"`
	doc    map[string]any
	vendor string
}

const (
	VendorClaude  = "claude"
	VendorCodex   = "codex"
	VendorCopilot = "copilot"
)

// Vendor uses the explicit hook vendor or Copilot's payload shape, defaulting to Claude.
func (in *Input) Vendor() string {
	if in == nil || in.vendor == "" {
		return VendorClaude
	}
	return in.vendor
}

// ParseInput never errors: malformed stdin yields a zero Input (fail-open).
func ParseInput(r io.Reader) *Input {
	raw, _ := io.ReadAll(r)
	in := &Input{Raw: raw}
	if json.Unmarshal(raw, &in.doc) != nil {
		return &Input{Raw: raw}
	}
	switch vendor := os.Getenv(hookspec.AgentEnv); vendor {
	case VendorClaude, VendorCodex, VendorCopilot:
		in.vendor = vendor
	}
	in.SessionID = in.nested("session_id")
	in.CWD = in.nested("cwd")
	in.TranscriptPath = in.nested("transcript_path")
	in.ToolName = in.nested("tool_name")
	in.HookEventName = in.nested("hook_event_name")
	in.StopHookActive, _ = in.doc["stop_hook_active"].(bool)
	in.Prompt = in.nested("prompt")
	in.AgentID = in.nested("agent_id")
	in.AgentTranscriptPath = in.nested("agent_transcript_path")
	if _, ok := in.doc["sessionId"].(string); ok && in.HookEventName == "" {
		in.vendor = VendorCopilot
	}
	if in.SessionID == "" {
		in.SessionID = in.nested("sessionId")
	}
	if in.ToolName == "" {
		in.ToolName = in.nested("toolName")
	}
	if in.CWD == "" {
		in.CWD = in.nested("workingDirectory")
	}
	if in.TranscriptPath == "" {
		in.TranscriptPath = in.nested("transcriptPath")
	}
	if in.Vendor() == VendorCopilot {
		in.normalizeToolArgs()
	}
	return in
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

func (in *Input) nested(path ...string) string {
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

// cloneToolInput preserves arguments while allowing a hook to replace individual keys.
func (in *Input) cloneToolInput() map[string]any {
	input := in.ToolInput()
	if input == nil {
		return nil
	}
	clone := make(map[string]any, len(input)+1)
	maps.Copy(clone, input)
	return clone
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
func MergeContext(event string, blocks []string, vendor ...string) string {
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
	payload := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": text}}
	if len(vendor) > 0 && vendor[0] == VendorCopilot {
		payload = map[string]any{"additionalContext": text}
	}
	out, _ := json.Marshal(payload)
	return string(out)
}

// Handler runs one hook.
type Handler func(stdin io.Reader, stdout, stderr io.Writer) int

// hookContext gives subprocesses the registered hook budget without changing process signal handling.
func hookContext(name string) (context.Context, context.CancelFunc) {
	for _, spec := range hookspec.Registry {
		if spec.Name == name {
			return context.WithTimeout(context.Background(), time.Duration(spec.Timeout)*time.Second)
		}
	}
	panic("unknown hook: " + name)
}

// Registry maps hook verbs to handlers; names must match hookspec.Registry.
var Registry = map[string]Handler{
	"bash-guard":           BashGuard,
	"docs-placement":       func(in io.Reader, _, errw io.Writer) int { return DocsPlacement(in, errw) },
	"session-start":        func(in io.Reader, out, _ io.Writer) int { return SessionStart(in, out) },
	"memory-recall":        func(in io.Reader, out, _ io.Writer) int { return MemoryRecall(in, out) },
	"wsl-path-normalize":   func(in io.Reader, out, _ io.Writer) int { return WSLPathNormalize(in, out) },
	"stop":                 func(in io.Reader, _, errw io.Writer) int { return Stop(in, errw) },
	"session-harvest-end":  func(in io.Reader, _, _ io.Writer) int { return SessionHarvestEnd(in) },
	"agent-effort":         func(in io.Reader, out, _ io.Writer) int { return AgentEffort(in, out) },
	"codex-agent-route":    CodexAgentRoute,
	"codex-routing-start":  CodexRoutingStart,
	"codex-routing-result": CodexRoutingResult,
	"codex-subagent-start": CodexSubagentStart,
	"workflow-lint":        func(in io.Reader, out, errw io.Writer) int { return WorkflowLint(in, out, errw) },
	"agent-inflight":       func(in io.Reader, _, errw io.Writer) int { return AgentInflight("pre", in, errw) },
	"agent-inflight-tag":   func(in io.Reader, _, errw io.Writer) int { return AgentInflight("tag", in, errw) },
	"agent-inflight-post":  func(in io.Reader, _, errw io.Writer) int { return AgentInflight("post", in, errw) },
	"plan-mirror":          func(in io.Reader, _, errw io.Writer) int { return PlanMirror(in, errw) },
	"save-plan":            func(in io.Reader, _, errw io.Writer) int { return SavePlan(in, errw) },
	"access-trace":         func(in io.Reader, _, _ io.Writer) int { return AccessTrace(in) },
	"subagent-stop":        func(in io.Reader, _, errw io.Writer) int { return SubagentStop(in, errw) },
	"pre-compact":          func(in io.Reader, _, _ io.Writer) int { return PreCompact(in) },
	"worktree-create":      func(in io.Reader, out, errw io.Writer) int { return WorktreeCreate(in, out, errw) },
	"worktree-remove":      func(in io.Reader, _, errw io.Writer) int { return WorktreeRemove(in, errw) },
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
