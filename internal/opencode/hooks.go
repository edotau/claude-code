package opencode

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

// hostedHooks are the registry's hook modules run around a headless `opencode run`; PreToolUse ones only detect.
var hostedHooks = hostedSet()

func hostedSet() map[string]bool {
	set := map[string]bool{}
	for _, s := range settings.SpecsFor("opencode", "hosted") {
		set[s.Name] = true
	}
	return set
}

// reasonLogRunes caps a hook's reason in the log line.
const reasonLogRunes = 300

// bindHooks is the hook set hostedBound filters; a seam so tests can bind a fake.
var bindHooks = hooks.Bind

// hostSeq makes each provisional session id unique within the process.
var hostSeq atomic.Uint64

// provisionalSession is the hook session id used until opencode mints one; unique per dispatch (time, pid, counter).
func provisionalSession() string {
	return fmt.Sprintf("oc-%s-%d-%d", time.Now().UTC().Format("20060102T150405"), os.Getpid(), hostSeq.Add(1))
}

// hookHost runs the hook phases around one headless opencode run; every method is a no-op on a nil host.
type hookHost struct {
	bound   []hooks.Bound
	cwd     string
	session string
	log     io.Writer
	mu      sync.Mutex
	seen    map[string]bool
}

// newHookHost is the host for req, or nil when hooks do not apply (a memory-update child, or nothing bound).
func newHookHost(req agent.RunOptions) *hookHost {
	if memory.IsUpdateChild() {
		return nil
	}
	bound := hostedBound(req.SafeMode)
	if len(bound) == 0 {
		return nil
	}
	cwd := req.WorkDir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	h := &hookHost{bound: bound, cwd: cwd, session: req.SessionID, seen: map[string]bool{}, log: openHookLog()}
	if h.session == "" {
		h.session = provisionalSession()
	}
	return h
}

// hostedBound is the hosted hooks; safeMode (--pure) keeps only the safety gate.
func hostedBound(safeMode bool) []hooks.Bound {
	var bound []hooks.Bound
	for _, b := range bindHooks() {
		if hostedHooks[b.Spec.Name] && (b.Gate || !safeMode) {
			bound = append(bound, b)
		}
	}
	return bound
}

// dropPlugin removes the specs the injected plugin already runs in-process, so no guard runs twice (no-op on nil).
func (h *hookHost) dropPlugin() {
	if h == nil {
		return
	}
	plugin := map[string]bool{}
	for _, s := range settings.SpecsFor("opencode", "plugin") {
		plugin[s.Name] = true
	}
	h.bound = slices.DeleteFunc(h.bound, func(b hooks.Bound) bool { return plugin[b.Spec.Name] })
}

// openHookLog appends to state/harness/opencode/hooks.log (0600); io.Discard when it cannot be opened.
func openHookLog() io.Writer {
	dir := filepath.Join(paths.StateDir(), "opencode")
	if os.MkdirAll(dir, 0o700) != nil {
		return io.Discard
	}
	f, err := os.OpenFile(filepath.Join(dir, "hooks.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return io.Discard
	}
	return f
}

// close releases the log file.
func (h *hookHost) close() {
	if c, ok := h.log.(io.Closer); ok {
		_ = c.Close()
	}
}

// note appends one line to the hook log: time, session, phase and the finding; never a prompt or command text.
func (h *hookHost) note(phase, format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(h.log, "%s %s phase=%s %s\n", time.Now().UTC().Format(time.RFC3339), h.session, phase, fmt.Sprintf(format, args...))
}

// fire runs each bound hook on event whose matcher hits tool, one Dispatch each so the log names the hook;
// it stops at the first Block and returns it as err (only the before phase acts on it).
func (h *hookHost) fire(phase, event, tool string, payload map[string]any) (ctx []string, err error) {
	payload["session_id"], payload["cwd"], payload["hook_event_name"] = h.session, h.cwd, event
	if tool != "" {
		payload["tool_name"] = tool
	}
	raw, _ := json.Marshal(payload)
	for _, b := range h.bound {
		if b.Spec.Event != event {
			continue
		}
		res, err := hooks.Dispatch([]hooks.Bound{b}, event, core.ParseInput(strings.NewReader(string(raw))), h.log)
		if err != nil {
			h.note(phase, "event=%s hook=%s error=%v", event, b.Spec.Name, err)
			continue
		}
		if res.Decision != core.Allow || res.Context != "" || res.Stderr != "" {
			h.note(phase, "event=%s tool=%s hook=%s decision=%s context_chars=%d reason=%q", event, tool, b.Spec.Name,
				[]string{"allow", "ask", "block"}[res.Decision], len(res.Context), textutil.CapRunes(cmp.Or(res.Reason, res.Stderr), reasonLogRunes, "…"))
		} else {
			h.note(phase, "event=%s tool=%s hook=%s decision=allow", event, tool, b.Spec.Name)
		}
		if res.Decision == core.Block {
			return ctx, fmt.Errorf("hook %s blocked the dispatch: %s", b.Spec.Name, cmp.Or(res.Reason, res.Stderr, "no reason given"))
		}
		if res.Context != "" {
			ctx = append(ctx, res.Context)
		}
	}
	return ctx, nil
}

// before runs SessionStart then UserPromptSubmit and returns argv with their context folded ahead of the prompt;
// a Block from either is the error, and the caller must not start the child.
func (h *hookHost) before(argv []string, prompt string) ([]string, error) {
	if h == nil {
		return argv, nil
	}
	ctx, err := h.fire("before", settings.EventSessionStart, "", map[string]any{})
	if err == nil {
		var more []string
		more, err = h.fire("before", settings.EventUserPromptSubmit, "", map[string]any{"prompt": prompt})
		ctx = append(ctx, more...)
	}
	if err != nil {
		h.close()
		return argv, err
	}
	if len(ctx) == 0 || len(argv) == 0 || argv[len(argv)-1] != prompt {
		return argv, nil
	}
	out := append([]string(nil), argv...)
	out[len(out)-1] = strings.Join(ctx, "\n\n") + "\n\n" + prompt
	return out, nil
}

// parser is the stream parser that hands each finished tool call to the during-run phase; onStep (may be nil)
// fires on every step_start.
func (h *hookHost) parser(onStep func()) func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error) {
	var onTool func(openCodeEvent)
	if h != nil {
		onTool = h.during
	}
	return func(br *bufio.Reader, sink wire.Sink, onMeaningful func(), logs ...wire.StreamLog) (wire.Result, error) {
		return parseOpenCodeEvents(br, sink, onMeaningful, onTool, onStep, logs...)
	}
}

// during is PreToolUse (detect-only) then PostToolUse for one finished tool call; its context is logged, not injected.
func (h *hookHost) during(ev openCodeEvent) {
	if ev.SessionID != "" {
		h.session = ev.SessionID
	}
	tool, input, ok := claudeTool(ev.Part.Tool, ev.Part.State.Input)
	if !ok || h.seen[ev.Part.CallID] {
		return
	}
	h.seen[ev.Part.CallID] = true
	// Blocks are detect-only here: fire has logged them and the tool call already ran.
	_, _ = h.fire("during", settings.EventPreToolUse, tool, map[string]any{"tool_input": input})
	_, _ = h.fire("during", settings.EventPostToolUse, tool, map[string]any{"tool_input": input, "tool_response": ev.Part.State.Output})
}

// after runs Stop then SessionEnd once the child has exited; res carries the session id opencode minted.
func (h *hookHost) after(res wire.Result) {
	if h == nil {
		return
	}
	defer h.close()
	if res.SessionID != "" {
		h.session = res.SessionID
	}
	h.mu.Lock()
	acted := len(h.seen) > 0
	h.mu.Unlock()
	if !acted { // no watched tool call ran, so there is nothing to format, tick or save (about 1 s per dispatch)
		return
	}
	// The child has exited, so a Block has nothing left to stop; fire has logged it.
	_, _ = h.fire("after", settings.EventStop, "", map[string]any{"stop_hook_active": false, "harness_agent": "opencode"})
	_, _ = h.fire("after", settings.EventSessionEnd, "", map[string]any{"reason": "other", "harness_agent": "opencode"})
}

// claudeTool maps an opencode tool call to the Claude tool name and input the hooks read; ok is false if none watches it.
func claudeTool(tool string, in map[string]any) (string, map[string]any, bool) {
	rename := func(name string, keys map[string]string) (string, map[string]any, bool) {
		out := map[string]any{}
		for from, to := range keys {
			if v, found := in[from]; found {
				out[to] = v
			}
		}
		return name, out, true
	}
	switch tool {
	case "bash":
		return rename("Bash", map[string]string{"command": "command", "description": "description", "timeout": "timeout"})
	case "write":
		return rename("Write", map[string]string{"filePath": "file_path", "content": "content"})
	case "edit":
		return rename("Edit", map[string]string{"filePath": "file_path", "oldString": "old_string", "newString": "new_string", "replaceAll": "replace_all"})
	case "task":
		return rename("Task", map[string]string{"prompt": "prompt", "description": "description", "subagent_type": "subagent_type"})
	}
	return "", nil, false
}
