// Package hooks implements `claude-code hook <Event> [--<module>|--all]`: JSON on stdin, exit 0 = proceed, 2 = block.
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

	"github.com/edotau/claude-code/internal/hooks/hookspec"
	"github.com/edotau/claude-code/internal/textutil"
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

// ToolCommand is the Bash command under tool_input (a top-level .command is the fallback).
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

// hookContext gives subprocesses the registered hook budget without changing process signal handling.
func hookContext(name string) (context.Context, context.CancelFunc) {
	for _, spec := range hookspec.Specs() {
		if spec.Name == name {
			return context.WithTimeout(context.Background(), spec.Budget)
		}
	}
	panic("unknown hook: " + name)
}

// Native maps every hook module to its Runner; names must match hookspec.Registry.
var Native = map[string]Runner{
	"safety-scan":               safetyScan,
	"rtk-rewrite":               rtkRewrite,
	"docs-placement":            viaStderr(docsPlacement),
	"memory-start":              sessionStart,
	"memory-recall":             memoryRecall,
	"paths-resolve":             nativePathResolver,
	"memory-update":             memoryUpdateStop,
	"review-simplicity":         reviewSimplicity,
	"format-stop":               stopFormatRun,
	"memory-end":                sessionUpdateEnd,
	"dispatch-effort":           dispatchPre,
	"codex-agent-route":         nativeCodexAgentRoute,
	"codex-routing-start":       nativeCodexRoutingStart,
	"codex-routing-result":      nativeCodexRoutingResult,
	"codex-subagent-start":      nativeCodexSubagentStart,
	"dispatch-failure":          releaseSpawn,
	"dispatch-workflow":         releaseSpawn,
	"docs-plan-gate":            viaStderr(checkPlanTemplate),
	"docs-plan-skeleton":        planSkeleton,
	"docs-plan-skeleton-prompt": planSkeleton,
	"docs-plan-tick":            planTick,
	"docs-plan-reminder":        planReminder,
	"docs-plan-mirror":          viaStderr(planMirror),
	"docs-plan-save":            viaStderr(savePlan),
	"dispatch-stop":             subagentStopRunner,
	"memory-compact":            preCompact,
	"worktree-create":           worktreeCreate,
	"worktree-remove":           viaStderr(worktreeRemove),
	"worktree-mode":             worktreeMode,
	"worktree-busy":             worktreeBusy,
}

// runDirect is `hook <Event> --<module>`: one Runner rendered for its own process. A module that blocks exits 2 with
// its reason on stderr, which is how a guard run by a vendor or by safety-scan's own entry blocks.
func runDirect(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	run, ok := Native[name]
	if !ok {
		return ExitProceed
	}
	in := ParseInput(stdin)
	res := run(in)
	if res.Decision == Block && specOf(name).Blocks {
		if name == "safety-scan" && in.Vendor() == VendorCopilot {
			fmt.Fprintln(stdout, OutputCopilot(hookspec.EventPreToolUse, res))
		}
		fmt.Fprintln(stderr, res.Reason)
		return ExitBlock
	}
	res.Context = textutil.CapRunes(res.Context, contextCapChars, "…")
	if !bareStdout[name] {
		return Emit(in.Vendor(), eventOf(name), res, stdout, stderr)
	}
	var out strings.Builder
	code := Emit(in.Vendor(), eventOf(name), res, &out, stderr)
	_, _ = io.WriteString(stdout, strings.TrimSuffix(out.String(), "\n"))
	return code
}

// bareStdout lists the modules whose own-process stdout never carried a trailing newline.
var bareStdout = map[string]bool{
	"review-simplicity": true, "docs-plan-skeleton": true, "docs-plan-skeleton-prompt": true,
	"docs-plan-tick": true, "docs-plan-reminder": true,
}

// specOf is the hookspec entry for a module; the zero spec when unknown.
func specOf(name string) hookspec.EventSpec {
	for _, s := range hookspec.Specs() {
		if s.Name == name {
			return s
		}
	}
	return hookspec.EventSpec{}
}

// eventOf names the hookspec event a handler is declared on.
func eventOf(name string) string { return specOf(name).Event }

// RunArgs is `hook <Event> [--<module>|--all]`; an unknown event or module fails closed only when it reads as a guard.
func RunArgs(event string, flags []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if !IsEvent(event) {
		return unknownHook(event, "unknown hook event", stderr)
	}
	switch {
	case len(flags) == 0:
		return RunEvent(event, false, stdin, stdout, stderr)
	case len(flags) == 1 && flags[0] == hookspec.AllFlag:
		return RunEvent(event, true, stdin, stdout, stderr)
	case len(flags) == 1 && strings.HasPrefix(flags[0], "--"):
		return runModule(event, strings.TrimPrefix(flags[0], "--"), stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "claude-code hook %s: want no flag, --all or one --<module>, got %q\n", event, flags)
	return 1
}

// runModule runs one module on its declared event; a group switched off in harnessHooks proceeds.
func runModule(event, name string, stdin io.Reader, stdout, stderr io.Writer) int {
	if _, ok := Native[name]; !ok || eventOf(name) != event {
		return unknownHook(name, "no module on "+event+" named", stderr)
	}
	if g, found := hookspec.GroupOf(name); found && !enabledGroups()(g.Name, g.Gate) {
		return ExitProceed
	}
	return runDirect(name, stdin, stdout, stderr)
}

// unknownHook fails closed when the unknown name reads as a guard; a stale vendor hook file must not drop its gate.
func unknownHook(name, what string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "claude-code hook: %s %q\n", what, name)
	for _, w := range []string{"guard", "gate", "safety", "scan", "placement"} {
		if strings.Contains(name, w) {
			return ExitBlock
		}
	}
	return 1
}
